package mora

// companion_http.go is the kernel half of the narrow companion listener (graph
// node N12). internal/companion owns the wire contract, the device registry and
// the HTTP surface; it is a leaf that may import only the standard library, so
// everything that needs the vault, the index or the connectors lives here,
// behind the three-method companion.Reader seam.
//
// The shape of this file follows from one rule: a phone gets PROJECTIONS, never
// tools. There is no dispatch table here and no route that names a tool, so
// widening the phone's reach is a code change in a reviewed file rather than an
// entry appended to a map. That is the whole difference from `mora serve http`,
// whose /call escape hatch is exactly the thing a device must not have.
//
// # Freshness is carried, not implied
//
// Every projection is built from healthOf(cfg, now) — the same fail-closed
// health kernel `mora doctor` and the brief read — and carries the per-connector
// freshness rows, the index arm and the write policy. A 200 with a degraded
// index says so in the body. Nothing here can report a source fresh that the
// kernel calls stale, because nothing here classifies: it translates.
//
// # What a request may never do
//
// A companion request never rebuilds the index. The memory count opens the
// read-only DSN directly instead of going through openIndexRO or ensureIndexDB,
// both of which can auto-heal by rebuilding — a phone that can trigger a rebuild
// can spend the Mac's disk and CPU with one unauthenticated-looking GET.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/companion"
	healthpkg "github.com/pyranthus-hq/mora/internal/health"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	synthesispkg "github.com/pyranthus-hq/mora/internal/synthesis"
)

// defaultCompanionPort is the companion listener's port. It is NOT
// defaultHTTPPort: the two servers are separate processes-worth of surface with
// disjoint credentials, and sharing a port would mean one of them could not
// start. N22 owns publishing this beyond the loopback interface.
const defaultCompanionPort = 7778

// companionContextEvidence is how many memories a context bundle carries. It is
// below companion.MaxContextEvidence so a bundle has headroom for the gaps and
// the prompt inside the projection bound.
const companionContextEvidence = 12

// ---------------------------------------------------------------------------
// mora companion serve
// ---------------------------------------------------------------------------

func cmdCompanionServe(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("companion serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", defaultCompanionPort, "loopback port to listen on")
	jsonOut := fs.Bool("json", false, "not supported: this subcommand runs a server")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"usage: mora companion serve [--port %d] (unexpected argument %q)", defaultCompanionPort, fs.Arg(0))
	}
	if *jsonOut {
		// Same refusal as `mora serve http`: a long-running server has no
		// document to emit, and starting one under --json would block a
		// machine caller forever waiting to parse stdout.
		return newCodedError(errCodeUsageUnknownFlag, nil,
			"mora companion serve does not support --json (it runs a server)")
	}
	if *port <= 0 || *port > 65535 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"invalid --port %d: must be between 1 and 65535", *port)
	}

	reg, cfg, err := companionRegistry(ctx)
	if err != nil {
		return err
	}
	srv, err := companion.NewServer(companion.ServerOptions{
		Addr:     fmt.Sprintf("%s:%d", companion.LoopbackHost, *port),
		Devices:  reg,
		Reader:   newCompanionReader(cfg),
		Writer:   newCompanionWriter(),
		Captures: companion.NewReservationStore(cfg.StateDir, companion.WithReservationClock(cfg.OperationClock)),
		Now:      cfg.OperationClock,
		Log:      stdout,
	})
	if err != nil {
		return err
	}
	return srv.Serve(ctx)
}

// ---------------------------------------------------------------------------
// The kernel reader
// ---------------------------------------------------------------------------

// companionReader implements companion.Reader over a resolved Config.
//
// It carries a short-lived Today cache. Today walks the whole vault through
// briefDigest, and a phone that polls — which is exactly what a phone does —
// would pay that walk per poll. The cache is per-reader rather than a package
// global so two listeners in one process (a test, a future embedding) do not
// share one.
type companionReader struct {
	cfg Config

	mu     sync.Mutex
	today  companion.TodayProjection
	key    string
	cached time.Time
	valid  bool
}

func newCompanionReader(cfg Config) *companionReader { return &companionReader{cfg: cfg} }

func (k *companionReader) now() time.Time { return k.cfg.OperationClock().UTC().Truncate(time.Second) }

// Health projects the kernel's health snapshot.
func (k *companionReader) Health(ctx context.Context) (companion.HealthProjection, error) {
	ctx = companionKernelContext(ctx)
	now := k.now()
	snapshot := healthOf(k.cfg, now)
	out := companion.NewHealthProjection()
	out.GeneratedAt = companionStamp(now)
	out.State = companionHealthState(snapshot.State)
	out.Policy = companionWritePolicy(k.cfg)
	out.Index = companion.IndexHealth{
		State:    companionIndexState(snapshot.Index.State),
		Memories: companionMemoryCount(ctx, k.cfg),
		BuiltAt:  companionOptionalStamp(snapshot.Index.IndexedAt),
	}
	out.Sources = companionFreshness(snapshot.Sources, out.GeneratedAt)
	if err := out.Validate(); err != nil {
		return companion.HealthProjection{}, err
	}
	return out, nil
}

// Today projects the brief's surfaced items.
//
// The ordering is the product claim: what needs attention, then what was
// committed, then what merely changed. Only companion.MaxTodayItems survive, and
// Truncated says so — three of three and three of nine are different statements
// and a phone must be able to tell them apart.
func (k *companionReader) Today(ctx context.Context) (companion.TodayProjection, error) {
	ctx = companionKernelContext(ctx)
	now := k.now()
	snapshot := healthOf(k.cfg, now)

	// The cache key is the index's own state, so a rebuild, a pending write or
	// a new commit stamp retires the entry immediately. The TTL is the floor
	// under everything the key cannot see — a vault file edited by hand, a
	// connector that landed without touching the index — so a stale answer is
	// bounded in time as well as in state.
	key := companionTodayCacheKey(snapshot)
	if cached, ok := k.cachedToday(key, now); ok {
		return cached, nil
	}

	out := companion.NewTodayProjection()
	out.GeneratedAt = companionStamp(now)
	out.Health = companion.HealthSummary{
		State:  companionHealthState(snapshot.State),
		Policy: companionWritePolicy(k.cfg),
	}
	out.Freshness = companionFreshness(snapshot.Sources, out.GeneratedAt)

	// The brief reads the typed commitment inventory, which reaches the index
	// through ensureIndexDB. Under the read-only marker that refuses instead of
	// rebuilding, and an empty Today under a health summary that already says
	// unhealthy is the honest answer — a rebuild triggered by a phone is not an
	// answer at all.
	if err := ctx.Err(); err != nil {
		return companion.TodayProjection{}, err
	}
	digest, err := companionBrief(ctx, k.cfg, now)
	switch {
	case companionDegraded(err):
	case err != nil:
		return companion.TodayProjection{}, err
	default:
		if err := ctx.Err(); err != nil {
			return companion.TodayProjection{}, err
		}
		candidates := companionTodayCandidates(digest)
		if len(candidates) > companion.MaxTodayItems {
			out.Items = candidates[:companion.MaxTodayItems]
			out.Truncated = true
		} else {
			out.Items = candidates
		}
	}
	if err := out.Validate(); err != nil {
		return companion.TodayProjection{}, err
	}
	k.storeToday(key, now, out)
	return out, nil
}

// companionBrief is briefDigest with the caller's context threaded through.
//
// It is a separate entry point rather than a change to briefDigest because
// briefDigest takes no context: its signature is shared with the CLI and the
// MCP tools, and widening it would put a context on six call sites that have
// nothing to say about one. The two builds are briefDigest's own — the delta
// preview, and the fixed-window fallback when the delta is empty — so a phone
// sees the same Today the terminal does, minus any repair.
func companionBrief(ctx context.Context, cfg Config, now time.Time) (Digest, error) {
	d, err := buildDigest(cfg, now, briefOpts{advance: false, perSourceCap: mcpDigestMaxItems, ctx: ctx})
	if err != nil {
		return Digest{}, err
	}
	if briefSurfacedItemCount(d) == 0 {
		fallback, fallbackErr := buildDigest(cfg, now, briefOpts{
			advance: false, sinceHours: briefFallbackWindowHours, perSourceCap: mcpDigestMaxItems, ctx: ctx,
		})
		if fallbackErr != nil {
			return Digest{}, fallbackErr
		}
		d = preserveBriefFallbackEmptyExplanation(d, fallback)
	}
	return d, nil
}

// companionTodayTTL bounds how long a Today answer may be reused when the index
// state has not moved. It is short because Today is the screen a phone opens on
// and a minute-old answer is fine; it is not zero because a poll loop must not
// be able to walk the vault once per poll.
const companionTodayTTL = 60 * time.Second

// companionTodayCacheKey is the index's identity, not a hash of the answer. A
// changed key means the vault moved under the last answer, so the entry is not
// merely old, it is wrong.
func companionTodayCacheKey(h Health) string {
	return fmt.Sprintf("%s|%s|%s|%d|%t|%s",
		h.State, h.Index.State, h.Index.IndexedAt, h.Index.PendingOps, h.Index.Blocked, h.Index.DirtySince)
}

func (k *companionReader) cachedToday(key string, now time.Time) (companion.TodayProjection, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.valid || k.key != key {
		return companion.TodayProjection{}, false
	}
	// A clock that moved backwards (a pinned test clock, an NTP step) expires
	// the entry rather than extending it indefinitely.
	age := now.Sub(k.cached)
	if age < 0 || age > companionTodayTTL {
		return companion.TodayProjection{}, false
	}
	return k.today, true
}

func (k *companionReader) storeToday(key string, now time.Time, out companion.TodayProjection) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.today, k.key, k.cached, k.valid = out, key, now, true
}

// Context answers one grounded query.
//
// A device chooses a mode and a query. It cannot choose a source, a tool, a
// budget or a limit: every one of those is a lever on how much of the vault a
// request touches, and a phone on a hostile network is not the right place to
// hold them.
func (k *companionReader) Context(ctx context.Context, req companion.ContextRequest) (companion.ContextBundle, error) {
	ctx = companionKernelContext(ctx)
	now := k.now()
	out := companion.NewContextBundle()
	out.GeneratedAt = companionStamp(now)
	out.Mode = req.Mode
	out.Query = req.Query

	var (
		evidence []synthesispkg.Evidence
		gaps     []string
		err      error
	)
	switch req.Mode {
	case companion.ModeThink:
		evidence, gaps, err = k.thinkContext(ctx, req, now)
	case companion.ModeSearch:
		evidence, gaps, err = k.searchContext(ctx, req, now)
	case companion.ModeMeetingPrep:
		evidence, gaps, err = k.meetingContext(ctx, req, now)
	default:
		// Unreachable: Unmarshal already rejected any mode outside the frozen
		// vocabulary. Refusing rather than falling through to a default mode
		// keeps "the kernel chose what ran" true even if the vocabulary grows.
		return companion.ContextBundle{}, fmt.Errorf("companion context: unsupported mode %q", req.Mode)
	}
	if err != nil {
		return companion.ContextBundle{}, err
	}

	snapshot := healthOf(k.cfg, now)
	out.Evidence = companionEvidence(evidence)
	out.Gaps = companionGaps(gaps)
	out.Freshness = companionFreshness(snapshot.Sources, out.GeneratedAt)
	out.SynthesisPrompt = companionSynthesisPrompt(req.Mode, req.Query, len(out.Evidence), len(out.Gaps))
	out.Health = companion.HealthSummary{
		State:  companionHealthState(snapshot.State),
		Policy: companionWritePolicy(k.cfg),
	}
	if err := out.Validate(); err != nil {
		return companion.ContextBundle{}, err
	}
	return out, nil
}

// companionIndexNotReadable is the gap a bundle carries when retrieval could not
// run without a repair. It is prose for a phone screen, and it is deliberately
// not an error: the request succeeded, the vault simply cannot answer it without
// work the caller forbade, and that distinction is the whole of the honesty
// contract.
const companionIndexNotReadable = "The search index is missing or unreadable, so nothing was retrieved. Run `mora index rebuild` on the Mac."

// companionKernelContext marks every kernel call this listener makes as
// read-only.
//
// It is the ONE place the marker is set. The previous shape asked
// companionRetrievalReady at the boundary and called the kernel only when the
// answer looked safe, which is a guess about which paths repair — and it was
// wrong twice, because meeting_prep reached ensureIndexDB through the commitment
// inventory and think reached healShareIndex through a subscribed corpus. It
// also raced: the index could go away between the check and the call. Marking
// the context instead makes the refusal a property of the repair site, which
// cannot be wrong about itself.
func companionKernelContext(ctx context.Context) context.Context { return withReadOnly(ctx) }

// companionDegraded reports whether err is the kernel declining to repair.
//
// It is a bundle-shaped answer, not a failure: the phone is told the vault could
// not look, which is a different claim from having looked and found nothing.
func companionDegraded(err error) bool { return errors.Is(err, ErrReadOnlyRepairNeeded) }

func (k *companionReader) thinkContext(ctx context.Context, req companion.ContextRequest, now time.Time) ([]synthesispkg.Evidence, []string, error) {
	res, err := buildThink(ctx, k.cfg, req.Query, req.Scope, companionContextEvidence, now)
	if companionDegraded(err) {
		return nil, []string{companionIndexNotReadable}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return res.Evidence, flattenThinkGaps(res.Gaps), nil
}

func (k *companionReader) searchContext(ctx context.Context, req companion.ContextRequest, now time.Time) ([]synthesispkg.Evidence, []string, error) {
	mems, err := hybridSearch(ctx, k.cfg, req.Query, req.Scope, companionContextEvidence)
	if companionDegraded(err) {
		return nil, []string{companionIndexNotReadable}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	// BasicGaps is the pure half of the think gap analysis: staleness, evidence
	// density, coverage holes. Search gets the honest floor rather than nothing,
	// because a bundle with no gaps array is a claim that nothing is missing.
	return synthesispkg.EvidenceFromMemories(mems, req.Query), flattenThinkGaps(synthesispkg.BasicGaps(mems, req.Query, now)), nil
}

// meetingContext builds the next meeting's brief, optionally narrowed to the
// attendee the query names.
func (k *companionReader) meetingContext(ctx context.Context, req companion.ContextRequest, now time.Time) ([]synthesispkg.Evidence, []string, error) {
	var filter map[string]bool
	degraded := false
	if name := strings.TrimSpace(req.Query); name != "" {
		resolved, err := resolveEntityFilter(ctx, k.cfg, name)
		if companionDegraded(err) {
			// The narrowing needs the entity graph; the brief itself does not.
			// Losing the narrowing is worth saying and not worth failing over.
			degraded = true
			err = nil
			resolved = nil
		}
		if err != nil {
			// An unresolvable name is not a failure: it is the honest answer
			// that the vault does not know this person, and it belongs in the
			// gaps rather than in a 503.
			return nil, []string{"No entity in the vault matches the name in this query, so the brief is not narrowed to them."}, nil
		}
		filter = resolved
	}
	brief, err := buildNextMeetingBrief(ctx, k.cfg, now, filter, 0, meetingBriefDefaultPerGuest)
	if companionDegraded(err) {
		// buildNextMeetingBrief reaches the commitment inventory, which reaches
		// ensureIndexDB. With no index there is no brief to give, and saying so
		// is the answer.
		return nil, []string{companionIndexNotReadable}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	evidence := []synthesispkg.Evidence{}
	for _, section := range brief.Sections {
		for _, line := range section.Lines {
			citation := line.Citation
			evidence = append(evidence, synthesispkg.Evidence{
				StableID:          citation.MemoryID(),
				Title:             section.Title,
				Snippet:           line.Text,
				CanonicalSourceID: citation.Channel(),
				Timestamp:         citation.Date(),
			})
			if len(evidence) >= companionContextEvidence {
				break
			}
		}
		if len(evidence) >= companionContextEvidence {
			break
		}
	}
	gaps := append([]string{}, brief.Gaps...)
	if degraded {
		gaps = append(gaps, companionIndexNotReadable)
	}
	if brief.Event == nil {
		gaps = append(gaps, "No upcoming meeting was found in the vault, so this bundle has no event to prepare for.")
	}
	return evidence, gaps, nil
}

// ---------------------------------------------------------------------------
// The kernel writer
// ---------------------------------------------------------------------------

// companionWriter implements companion.Writer: the one path by which a phone can
// change the vault (graph node N21).
//
// # It opens no second door
//
// Publish dispatches through mcppkg.MutationAction and then through the SAME two
// destinations the MCP write_memory tool uses — stageMCPWriteProposal for
// `propose`, mcpWriteMemoryWith for `open` — which in turn use createMemory, the
// create-exclusive publish `mora write` uses. Nothing here touches the vault
// directly. That matters beyond tidiness: the governed write path is where the
// pending-op marker, the index upsert and the authored-write reconciliation
// live, and a listener that wrote its own file would produce a memory the index
// never learns about.
//
// The policy interpretation is mcppkg.MutationAction's and is not restated here.
// N21 consumes the write policy; it does not own it.
//
// # Exactly once, because the id is pinned
//
// Publish takes the vault id the capture must land on. The companion side
// derives it before it reserves the key, so every attempt at the same capture
// aims at the same path, and createMemory's create-exclusive publish decides the
// race: the first one links, and every later one is told the memory already
// exists. That is what closes the crash window between the publication and the
// receipt — a window a reservation alone cannot close, because the reservation
// is written before the write and says nothing about whether it landed.
//
// # Durable before it is claimed
//
// A vault write is a rename, and a rename is atomic without being durable: the
// file's bytes and its directory entry can still be in cache when this function
// returns. A receipt that says `applied` is a promise about stable storage, so
// the file and its parent are synced HERE, before the outcome goes back to the
// listener and long before the receipt settles.
//
// # The config is re-read per request, and a config that cannot be read is readonly
//
// Not captured at startup. An operator who runs `mora config mcp-write-policy
// readonly` while the listener is up has made a security decision, and a
// listener that answered from a policy it read at boot would keep accepting
// captures until someone restarted it.
//
// A config that cannot be read at all fails CLOSED, to a `rejected: policy`
// receipt rather than an error. The error shape was worse than untidy: it became
// a 503, the reservation stayed pending, and an unreadable vault turned every
// capture into a claim nothing could ever settle.
//
// # The read-only marker is deliberately NOT set
//
// companionKernelContext marks every READ this listener makes as "answer from
// what exists, never repair", because a repair is minutes of disk reachable from
// one request. Capture is the exception by definition: it is a write, it is
// governed by the vault's own policy, and marking it read-only would make the
// one authorized mutation refuse itself.
type companionWriter struct {
	// census bounds the published store without walking it per capture. It is
	// per writer rather than package-level so two listeners in one process — a
	// test, a future embedding — do not share one.
	census *publishedCensus
}

func newCompanionWriter() *companionWriter {
	return &companionWriter{census: newPublishedCensus()}
}

// RecordReceipt stores the bytes a published capture answered with, and is the
// only place the published store is trimmed.
//
// It is called AFTER the reservation has settled, which is what makes two things
// true at once: a rejected request never trims — so a refusal leaves the store
// byte-identical — and the trim never evicts a publication whose capture has not
// finished, because an unsettled record has no response bytes and the trim skips
// those.
func (w *companionWriter) RecordReceipt(ctx context.Context, id companion.CaptureIdentity, response []byte) error {
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	if err := recordCaptureReceipt(cfg, id, response); err != nil {
		return err
	}
	record, found, err := readCapturePublicationAt(capturePublicationKeyPath(cfg, id.DeviceID, id.Key))
	if err != nil {
		return err
	}
	if found {
		w.census.note(record)
	}
	w.census.trim(cfg, id)
	return nil
}

// Policy reports the vault's current write policy.
func (w *companionWriter) Policy(ctx context.Context) (companion.WritePolicy, error) {
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return "", err
	}
	return companionWritePolicy(cfg), nil
}

// Published reports whether the pinned id is already in the vault, and if so the
// outcome that describes it.
//
// It is asked only when a crashed reservation is reclaimed. The create-exclusive
// publish would catch the duplicate anyway; asking first is what turns "the
// retry cannot duplicate" into "the retry does not even try", and it is what
// lets a recovered capture settle its receipt without a second write.
//
// The outcome names `open` whatever the policy is NOW, because the memory exists
// and could only have been published under `open`. Binding the receipt to a
// policy that has since tightened would make it describe an outcome that never
// happened.
//
// A config that cannot be read answers "not published" rather than failing: the
// caller's next step is Publish, which fails closed on the same unreadable
// config, so the refusal happens once and in one place.
func (w *companionWriter) Published(ctx context.Context, c companion.Capture, id companion.CaptureIdentity) (companion.WriteOutcome, bool, error) {
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return companion.WriteOutcome{}, false, nil
	}
	state, err := w.verifyPublication(cfg, c, id)
	switch {
	case err != nil:
		return companion.WriteOutcome{}, false, err
	case state == publicationAbsent:
		return companion.WriteOutcome{}, false, nil
	case state == publicationForeign:
		// The pinned id is taken by something that is not this capture. Reporting
		// it published would hand the phone a receipt for somebody else's memory;
		// reporting it absent would send the retry into a write that cannot land.
		// It is settled as a rejection so the key stops being retried at all.
		return w.integrityFailure(cfg, id), true, nil
	}
	return companion.WriteOutcome{
		Policy:   companion.PolicyOpen,
		State:    companion.ReceiptApplied,
		MemoryID: companionOpaqueID(companion.PrefixMemory, id.MemoryID),
	}, true, nil
}

// publicationState is what the vault holds at a pinned id.
type publicationState int

const (
	// publicationAbsent: nothing is there, so the write may proceed.
	publicationAbsent publicationState = iota
	// publicationOurs: the memory there IS this capture, verified rather than
	// assumed.
	publicationOurs
	// publicationForeign: something is there and it is not this capture.
	publicationForeign
)

// verifyPublication decides which of the three it is.
//
// # Why EEXIST is not enough on its own
//
// Round two treated "the file is already there" as "the capture is already
// published". That is only true if the file is THIS capture's, and nothing
// checked: a tampered vault, or a 32-bit suffix collision between two different
// captures, produced a confident `applied` receipt for a memory the phone never
// wrote. The suffix is 32 bits; including the device in the preimage stops
// identical preimages, not collisions.
//
// So the id's ownership is recorded when it is claimed — a small sidecar under
// the state directory naming the device, the key and the capture identity — and
// the file itself is compared against what this capture would have written.
// Both have to agree:
//
//   - the sidecar says this identity owns the id, and
//   - the memory on disk carries this capture's own text, scope, type and the
//     companion source stamp.
//
// A sidecar that is missing is not a failure: a state directory can be moved or
// rebuilt independently of the vault, and refusing there would reject a memory
// the user really did capture. The file comparison still has to pass, so a
// missing sidecar narrows the check rather than skipping it.
func (w *companionWriter) verifyPublication(cfg Config, c companion.Capture, id companion.CaptureIdentity) (publicationState, error) {
	existing, err := findMemoryRaw(cfg, id.MemoryID)
	if err != nil {
		return publicationAbsent, nil
	}
	owner, found, err := readCapturePublication(cfg, id.MemoryID)
	if err != nil {
		return publicationForeign, nil
	}
	if found && !owner.matches(id) {
		return publicationForeign, nil
	}
	want, err := mcpMemoryFromArgs(companionWriteArgs(c), mcpWriteClock())
	if err != nil {
		return publicationForeign, nil
	}
	if existing.Text != want.Text || existing.Scope != want.Scope ||
		existing.Type != want.Type || existing.Source != want.Source {
		return publicationForeign, nil
	}
	return publicationOurs, nil
}

// integrityFailure is the outcome for a pinned id the vault holds against
// somebody else.
//
// The reason is `internal` because N02's reject_reason vocabulary is frozen and
// this is not a client condition: nothing the phone sent was wrong, and no other
// published reason describes "the vault holds a file where this capture belongs".
//
// The specific cause travels on the OUTCOME rather than being written here. The
// kernel has no log of its own on this path, and giving it one meant handing it
// the listener's stdout — which put a request-caused line on the surface N12
// promises is silent per request. The listener owns its log; this hands it a
// fact and lets it decide.
func (w *companionWriter) integrityFailure(cfg Config, id companion.CaptureIdentity) companion.WriteOutcome {
	return companion.WriteOutcome{
		Policy: companion.WritePolicy(configMCPWritePolicy(cfg)),
		State:  companion.ReceiptRejected,
		Reason: companion.ReasonInternal,
		IntegrityDetail: fmt.Sprintf("the vault holds a memory at %s that is not this capture (device %s)",
			id.MemoryID, id.DeviceID),
	}
}

// PublishedForKey reports what this device's key has already published.
//
// It is asked before a FRESH reservation is taken, and it is the answer the
// reservation store cannot give: a capture killed after its publication leaves a
// pending row the sweep collects, and after that the key looks unused. Without
// this lookup a re-stamped retry of such a capture is a new identity, a new
// derived id, and a second memory.
func (w *companionWriter) PublishedForKey(ctx context.Context, deviceID, key string) (string, bool, []byte, error) {
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		// Fail closed the way Publish does: an unreadable config is not a licence
		// to treat a used key as fresh, and the write that follows will refuse
		// anyway.
		return "", false, nil, nil
	}
	record, found, err := publishedForKey(cfg, deviceID, key)
	if err != nil || !found {
		return "", false, nil, err
	}
	return record.Identity, true, []byte(record.Response), nil
}

// Publish runs one capture through the kernel's governed write path, pinned to
// memoryID, and reports what actually happened to the vault.
func (w *companionWriter) Publish(ctx context.Context, c companion.Capture, id companion.CaptureIdentity) (companion.WriteOutcome, error) {
	started := time.Now()
	configStarted := time.Now()
	cfg, err := loadConfigFor(ctx)
	configMillis := time.Since(configStarted).Milliseconds()
	if err != nil {
		// Fail CLOSED. A policy that cannot be read is readonly, and readonly
		// rejects — the only direction that cannot describe a write as permitted.
		// It is a terminal receipt rather than an error so the reservation
		// SETTLES: a pending claim over an unreadable config is a claim nothing
		// will ever resolve.
		return companion.WriteOutcome{
			Policy: companion.PolicyReadonly,
			State:  companion.ReceiptRejected,
			Reason: companion.ReasonPolicy,
		}, nil
	}
	policy := configMCPWritePolicy(cfg)
	out := companion.WriteOutcome{Policy: companion.WritePolicy(policy)}

	// The usage trace and the invocation below are the SAME ones invokeMCPTool
	// builds. The capture cannot go through invokeMCPTool itself — the MCP tool
	// table pins the handler signature, so the pinned id has nowhere to travel —
	// so it produces the identical ledger row by using the identical type. A
	// capture that wrote the vault and left no row would make the usage ledger a
	// record of some writes rather than of writes.
	trace := &mcpUsageTrace{configMillis: configMillis}
	traced := context.WithValue(ctx, mcpUsageTraceKey{}, trace)
	args := companionWriteArgs(c)

	var (
		value     any
		writeErr  error
		integrity bool
		claim     publicationClaim
	)
	action, policyErr := mcppkg.MutationAction(policy, "write_memory")
	switch action {
	case mcppkg.ActionRefuse:
		// readonly. Nothing is staged and nothing is written, so the refusal is
		// terminal and carries the published policy reason.
		writeErr = policyErr
	case mcppkg.ActionPropose:
		// propose. The capture is staged in the same pending queue
		// `mora mcp proposals` already lists and approves, so a phone capture and
		// an agent write wait in one place under one review. Nothing is in the
		// vault, which is exactly what an `accepted` receipt claims.
		value, writeErr = stageMCPWriteProposal(cfg, args)
	default:
		// open. The receipt flips to applied only on the far side of this call.
		//
		// The id's ownership is recorded BEFORE the write, so a later attempt at
		// the same pinned id can tell "this is mine" from "something else is
		// there". Recording it after would leave a window in which our own memory
		// looked foreign to our own retry.
		var claimErr error
		if claim, claimErr = claimCapturePublication(cfg, id); claimErr != nil {
			if errors.Is(claimErr, errMemoryIDMismatch) {
				// The id is owned by a different capture. Nothing was created,
				// so nothing has to be taken back.
				integrity = true
				break
			}
			return companion.WriteOutcome{}, claimErr
		}
		value, writeErr = mcpWriteMemoryWith(traced, cfg, args, withExplicitMemoryID(id.MemoryID))
	}
	// Some handlers, mutations included, have no tool-specific structural counts.
	// They still get one content-free event and an honest envelope size — the
	// same five lines invokeMCPTool runs for the same reason.
	if trace.event.Tool == "" {
		results := 0
		if writeErr == nil {
			results = 1
		}
		trace.event = usageEvent{Tool: "write_memory", Results: results}
	}
	_ = mcpToolInvocation{cfg: cfg, started: started, trace: trace, value: value, err: writeErr, loggable: true}.result()

	switch action {
	case mcppkg.ActionRefuse:
		out.State = companion.ReceiptRejected
		out.Reason = companion.ReasonPolicy
		return out, nil
	case mcppkg.ActionPropose:
		if writeErr != nil {
			return companion.WriteOutcome{}, writeErr
		}
		out.State = companion.ReceiptAccepted
		return out, nil
	}
	if integrity {
		return w.integrityFailure(cfg, id), nil
	}
	if writeErr != nil {
		return companion.WriteOutcome{}, writeErr
	}
	memory, ok := companionWrittenMemory(value)
	if !ok {
		// The write path answered in a shape this function does not understand.
		// Reporting applied without a memory id would be the one claim a receipt
		// may never make, and Receipt.Validate would refuse it anyway.
		return companion.WriteOutcome{}, fmt.Errorf("companion capture: the governed write path returned no memory")
	}
	// A pinned id the vault already holds is only OUR publication if the file
	// says so. The create-exclusive publish reports EEXIST without reading
	// anything, so the reading happens here, and a file that is not this
	// capture's is a rejection rather than a confident `applied`.
	if companionAlreadyPublished(value) {
		state, verr := w.verifyPublication(cfg, c, id)
		if verr != nil {
			return companion.WriteOutcome{}, verr
		}
		if state != publicationOurs {
			// The id is ours by the record and foreign by the file: a tampered or
			// collided vault. The ownership this call created goes back, so the
			// rejection leaves the state directory exactly as it found it — and a
			// caller who can grind the suffix cannot pre-plant ownership for ids
			// nobody has published.
			// Exactly what this request created goes back, and nothing else. A
			// record it merely found — somebody else's, or its own from an
			// earlier attempt — is not its to remove.
			if rerr := claim.rollback(); rerr != nil {
				return companion.WriteOutcome{}, rerr
			}
			return w.integrityFailure(cfg, id), nil
		}
	}
	// Durability BEFORE the claim. A sync failure is a failure of the whole
	// publication: the bytes may be there and may not, and the honest answer is
	// to leave the reservation unsettled so a retry re-runs the check. The retry
	// is cheap and safe — the pinned id means it finds the memory already
	// published rather than writing a second one.
	if err := companionSyncPublication(memory.Path); err != nil {
		return companion.WriteOutcome{}, fmt.Errorf("companion capture: the memory was written but could not be made durable: %w", err)
	}
	out.State = companion.ReceiptApplied
	out.MemoryID = companionOpaqueID(companion.PrefixMemory, memory.ID)
	return out, nil
}

// companionSyncPublication makes a just-published memory durable, and is the
// seam a test replaces to prove the ordering.
//
// It is a package variable rather than a direct call for one reason: the claim
// under test is that the sync happens BEFORE the receipt settles, and an
// ordering claim needs something that can observe when it ran. Production never
// replaces it.
var companionSyncPublication = syncPublication

// syncPublication fsyncs a file and the directory entry that points at it.
//
// Both halves are needed and neither is enough alone. Syncing the file leaves
// the directory entry in cache, so a crash can lose the name while keeping the
// bytes; syncing the directory alone leaves the bytes in cache. internal/atomicio
// owns the platform split for the directory half (it is a no-op on Windows,
// where NTFS journals the rename's metadata), so this calls it rather than
// re-deciding it.
//
// The file is opened O_WRONLY rather than with os.Open, and that is not a
// stylistic choice. Go's File.Sync is FlushFileBuffers on Windows, which needs a
// handle carrying GENERIC_WRITE; a read-only handle fails with "Access is
// denied" — which is exactly how this arrived, as a Windows-only CI failure
// against a path that works on POSIX. Opening for write syncs nothing extra: no
// bytes are written through the handle.
func syncPublication(path string) error {
	if path == "" {
		return fmt.Errorf("companion capture: the governed write path returned no memory path to sync")
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return atomicio.SyncDir(filepath.Dir(path))
}

// companionWrittenMemory pulls the saved memory out of the governed write path's
// result. Both of that path's success shapes — the plain one and the
// index-degraded one — carry it under the same key, which is why the degraded
// case is still an applied receipt: the vault has the memory, and only the
// derived index is behind.
func companionWrittenMemory(result any) (Memory, bool) {
	wrapped, ok := result.(map[string]any)
	if !ok {
		return Memory{}, false
	}
	memory, ok := wrapped["memory"].(Memory)
	if !ok || memory.ID == "" {
		return Memory{}, false
	}
	return memory, true
}

// companionWriteArgs shapes a capture as a write_memory request.
//
// Three fields are stamped by the kernel and are NOT readable from the capture:
//
//   - source is "companion", always. It is the provenance an operator reads to
//     know a memory came from a phone, and a device that could set it could
//     make its writes look like the CLI's.
//   - type is an insight. A phone captures a note; the decision fields carry
//     validity semantics the companion contract does not model, and letting a
//     device reach them would put half a decision record on the wire.
//   - title is derived from the text rather than supplied, because the capture
//     schema has no title and inventing a second free-text field on the wire is
//     how a bounded contract stops being bounded.
//
// scope IS the device's, because it is the one placement decision the phone is
// entitled to make, and the schema already restricts it to "personal" or
// "project:<name>" — a device cannot name a source or reach another vault.
func companionWriteArgs(c companion.Capture) map[string]any {
	return map[string]any{
		"scope":  c.Scope,
		"type":   "insight",
		"title":  companionCaptureTitle(c.Text),
		"text":   c.Text,
		"source": companion.OriginCompanion,
	}
}

// companionCaptureTitle derives a bounded title from the captured text.
//
// It is the first non-empty line, truncated on a rune boundary. A capture with
// nothing but whitespace cannot occur — the schema requires text — but a capture
// whose first line is whitespace can, so the fallback is a fixed string rather
// than an empty title the write path would refuse.
func companionCaptureTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		return companionText(trimmed, companionCaptureTitleBytes)
	}
	return "Captured note"
}

// companionCaptureTitleBytes bounds a derived title. It is well under the
// vault's own limits and short enough to read in a list.
const companionCaptureTitleBytes = 120

// ---------------------------------------------------------------------------
// Today assembly
// ---------------------------------------------------------------------------

// companionTodayCandidates flattens a digest into ranked Today items. It returns
// EVERY candidate; the caller decides how many survive, so Truncated is computed
// against the real total rather than against a pre-trimmed list.
func companionTodayCandidates(d Digest) []companion.TodayItem {
	seen := map[string]bool{}
	out := []companion.TodayItem{}
	add := func(item DigestItem, kind companion.TodayItemKind) {
		if item.ID == "" || seen[item.ID] {
			return
		}
		seen[item.ID] = true
		converted, ok := companionTodayItem(item, kind)
		if !ok {
			return
		}
		out = append(out, converted)
	}
	for _, item := range d.Urgent {
		add(item, companion.ItemNeedsAttention)
	}
	for _, section := range d.Sections {
		for _, item := range section.Items {
			if len(item.Obligations) > 0 {
				add(item, companion.ItemCommitment)
			}
		}
	}
	for _, section := range d.Sections {
		for _, item := range section.Items {
			add(item, companion.ItemChanged)
		}
	}
	return out
}

func companionTodayItem(item DigestItem, kind companion.TodayItemKind) (companion.TodayItem, bool) {
	source := companionSourceKey(item.Source)
	if source == "" {
		// A row with no attributable source cannot carry evidence, and an item
		// without evidence is not shippable.
		return companion.TodayItem{}, false
	}
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = "(untitled)"
	}
	return companion.TodayItem{
		ID:    companionOpaqueID("itm_", item.ID),
		Kind:  kind,
		Title: companionText(title, companion.MaxTitleBytes),
		Body:  companionText(companionItemBody(item), companion.MaxBodyBytes),
		Evidence: []companion.Evidence{{
			MemoryID:   companionOpaqueID(companion.PrefixMemory, item.ID),
			Source:     source,
			OccurredAt: companionOptionalStamp(item.CreatedAt),
			Snippet:    companionText(item.Snippet, companion.MaxSnippetBytes),
		}},
	}, true
}

// companionItemBody prefers the commitment summary when the item carries one:
// "you owe Sam the deck by Friday" is the thing worth reading on a phone, and
// the artifact snippet is one tap away behind the evidence row.
func companionItemBody(item DigestItem) string {
	if len(item.Obligations) > 0 {
		parts := make([]string, 0, len(item.Obligations))
		for _, ob := range item.Obligations {
			summary := strings.TrimSpace(ob.Summary)
			if summary == "" {
				continue
			}
			if due := strings.TrimSpace(ob.DueAt); due != "" {
				summary += " (due " + due + ")"
			}
			parts = append(parts, summary)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return item.Snippet
}

// ---------------------------------------------------------------------------
// Translation helpers
// ---------------------------------------------------------------------------

// companionEvidence converts retrieval evidence, dropping any row that cannot
// carry a valid citation. Dropping is correct: an evidence row whose id or
// source will not validate is a citation a phone cannot check, and the
// alternative — fabricating a placeholder — is the failure mode the whole
// evidence discipline exists to prevent.
func companionEvidence(rows []synthesispkg.Evidence) []companion.Evidence {
	out := []companion.Evidence{}
	for _, row := range rows {
		if len(out) >= companion.MaxContextEvidence {
			break
		}
		source := companionSourceKey(row.CanonicalSourceID)
		if source == "" || row.StableID == "" {
			continue
		}
		occurred := companionOptionalStamp(row.Timestamp)
		if occurred == "" {
			occurred = companionOptionalStamp(row.CreatedAt)
		}
		out = append(out, companion.Evidence{
			MemoryID:   companionOpaqueID(companion.PrefixMemory, row.StableID),
			Source:     source,
			OccurredAt: occurred,
			Snippet:    companionText(row.Snippet, companion.MaxSnippetBytes),
			DeepLink:   companionText(row.DeepLink, companion.MaxDeepLinkBytes),
		})
	}
	return out
}

func companionGaps(gaps []string) []string {
	out := []string{}
	for _, gap := range gaps {
		if len(out) >= companion.MaxGaps {
			break
		}
		trimmed := companionText(strings.TrimSpace(gap), companion.MaxGapBytes)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func flattenThinkGaps(g synthesispkg.Gaps) []string {
	out := []string{}
	for _, family := range [][]string{
		g.CoverageHoles, g.Stale, g.FreshnessUnknown, g.SparseEvidence,
		g.SourceCoverage, g.TemporalState, g.ThinCoverage, g.RetrievalCaveats,
	} {
		out = append(out, family...)
	}
	return out
}

// companionSynthesisPrompt is instructions, never content.
//
// The classic shape — a prompt with the evidence pasted into it — would double
// every snippet inside a bounded projection and push a full bundle past
// MaxSynthesisPromptBytes on a normal query. The bundle already carries the
// evidence and the gaps as structured fields, so the prompt refers to them.
func companionSynthesisPrompt(mode companion.ContextMode, query string, evidence, gaps int) string {
	var task string
	switch mode {
	case companion.ModeSearch:
		task = "List what the evidence below actually says about this query."
	case companion.ModeMeetingPrep:
		task = "Prepare the reader for this meeting using only the evidence below."
	default:
		task = "Answer this question using only the evidence below."
	}
	prompt := fmt.Sprintf(`%s

Question: %s

You have %d evidence rows and %d recorded gaps in this bundle.
Rules:
- Cite the memory_id of every row you use. A claim with no citation does not ship.
- Do not use anything outside this bundle, and do not fill a gap by guessing.
- State the gaps plainly at the end; they are what the vault does not know.
- If the evidence does not answer the question, say so and stop.`,
		task, companionText(query, companion.MaxQueryBytes), evidence, gaps)
	return companionText(prompt, companion.MaxSynthesisPromptBytes)
}

// companionFreshness translates the kernel's per-connector rows.
//
// The age is recomputed from the two timestamps rather than copied from
// AgeHours: the contract requires age_seconds to be EXACTLY the distance from
// the source's last success to the moment the projection was generated, with no
// rounding, because that number is what a phone renders as "15 minutes ago".
func companionFreshness(sources []healthpkg.Source, generatedAt string) []companion.SourceFreshness {
	out := []companion.SourceFreshness{}
	for _, src := range sources {
		if len(out) >= companion.MaxFreshnessSources {
			break
		}
		key := companionSourceKey(src.Key)
		if key == "" {
			continue
		}
		out = append(out, companionFreshnessRow(key, src, generatedAt))
	}
	return out
}

// companionFreshnessRow translates ONE kernel row.
//
// The shape that used to be wrong: a failure with no successful sync behind it
// — which is what an unreadable sources.json produces, state failed with an
// empty last_success_at and a data.corrupt code — collapsed to plain "never"
// and dropped the code on the way. A phone then showed a source that had simply
// never run, rather than one that is broken right now and says why. Failure
// survives the absence of a timestamp; only fresh and stale depend on one,
// because only they make a claim about age.
func companionFreshnessRow(key string, src healthpkg.Source, generatedAt string) companion.SourceFreshness {
	row := companion.SourceFreshness{Key: key, AgeSeconds: -1}
	last := companionOptionalStamp(src.LastSuccessAt)

	switch src.State {
	case healthpkg.Failed:
		// A failed row keeps its state and its code whether or not it ever
		// succeeded. The contract REQUIRES a code here, so an untyped failure
		// becomes internal rather than an unexplained red dot.
		row.State = companion.FreshnessFailed
		row.ErrorCode = companionSourceErrorCode(src.ErrorCode)
		row.LastSuccessAt = last
		if last != "" {
			row.AgeSeconds = companionAgeSeconds(last, generatedAt)
		}
	case healthpkg.Fresh, healthpkg.Stale:
		if last == "" {
			// The contract forbids a fresh or stale row without the timestamp
			// its age is measured from, and inventing one would be the lie the
			// exactness rule exists to prevent. "Never successfully synced" is
			// what an absent last success actually means.
			row.State = companion.FreshnessNever
			break
		}
		row.LastSuccessAt = last
		row.AgeSeconds = companionAgeSeconds(last, generatedAt)
		if row.AgeSeconds < 0 {
			// The stored success is later than this projection's own clock — a
			// skewed connector stamp. A negative age fails validation and
			// reporting the source as never-successful would be a lie; age
			// zero, "it succeeded as of now", is the honest floor.
			row.LastSuccessAt = generatedAt
			row.AgeSeconds = 0
		}
		if src.State == healthpkg.Fresh {
			row.State = companion.FreshnessFresh
			// A fresh source carries no error code, by contract.
			break
		}
		row.State = companion.FreshnessStale
		// A stale row MAY name why, and staleness usually has a reason worth
		// showing, so the code survives when the kernel typed one.
		if src.ErrorCode != "" {
			row.ErrorCode = companionSourceErrorCode(src.ErrorCode)
		}
	default:
		// never, and anything the kernel grows later: no claim, no age, no code.
		row.State = companion.FreshnessNever
	}
	return row
}

func companionAgeSeconds(lastSuccessAt, generatedAt string) int64 {
	last, lerr := time.Parse(time.RFC3339, lastSuccessAt)
	generated, gerr := time.Parse(time.RFC3339, generatedAt)
	if lerr != nil || gerr != nil {
		return -1
	}
	return int64(generated.Sub(last).Seconds())
}

// companionSourceErrorCode maps the CLI's published error-code taxonomy onto the
// frozen companion vocabulary. Anything unrecognized becomes "internal" rather
// than passing through: the companion vocabulary is frozen precisely so a
// connector's own code can never reach a phone, and a default that forwards the
// input would undo that on the first new code.
func companionSourceErrorCode(code string) companion.SourceErrorCode {
	switch code {
	case errCodeConnectorUnauthorized:
		return companion.ErrAuthExpired
	case errCodeConsentRequired:
		return companion.ErrPermissionDenied
	case errCodeConnectorUnavailable, errCodeConnectorEmpty, errCodeConnectorStale:
		return companion.ErrSourceUnavailable
	case errCodeDataNotFound:
		return companion.ErrNotConfigured
	}
	return companion.ErrInternal
}

func companionHealthState(state string) companion.HealthState {
	switch state {
	case healthHealthy:
		return companion.HealthHealthy
	case healthDegraded:
		return companion.HealthDegraded
	}
	return companion.HealthUnhealthy
}

// companionIndexState collapses the kernel's five-state index arm onto the
// contract's three-state vocabulary. The table is N02's, published in
// docs/companion-contract.md, and it is deliberately NOT the kernel's own
// aggregate collapse:
//
//	fresh    -> healthy      the index matches the vault
//	dirty    -> degraded     behind the vault: usable and incomplete
//	degraded -> degraded     straight through
//	failed   -> unhealthy    could not be opened, or does not match this build
//	never    -> unhealthy    there is no index, so nothing can be retrieved
//
// internal/health folds dirty into unhealthy, because its aggregate is
// `mora doctor`'s fail-closed verdict on whether the vault can be trusted at
// all. This is one ARM of a projection that already carries that aggregate
// beside it in the top-level state, and collapsing a behind-but-usable index
// into the same value as a missing one would cost the phone the distinction
// between "you are seeing slightly old results" and "there is nothing to see".
func companionIndexState(state string) companion.HealthState {
	switch state {
	case healthpkg.IndexFresh:
		return companion.HealthHealthy
	case healthpkg.IndexDirty, healthpkg.IndexDegraded:
		return companion.HealthDegraded
	}
	return companion.HealthUnhealthy
}

func companionWritePolicy(cfg Config) companion.WritePolicy {
	return companion.WritePolicy(configMCPWritePolicy(cfg))
}

// companionMemoryCount counts indexed memories WITHOUT the auto-heal paths.
//
// openIndexRO and ensureIndexDB both rebuild a stale or missing index, and a
// rebuild is minutes of disk and CPU. Reachable from a network request, that is
// a denial of service with a valid credential. A count that cannot be read is
// reported as zero beside an index state that is already unhealthy, which is the
// fail-closed answer.
func companionMemoryCount(ctx context.Context, cfg Config) int {
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		return 0
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories`).Scan(&n); err != nil || n < 0 {
		return 0
	}
	return n
}

// companionStamp renders the contract's timestamp: UTC, second precision, Z.
func companionStamp(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// companionOptionalStamp normalizes a stored timestamp, or returns "" when it
// cannot be parsed. Returning "" rather than the raw string is what keeps a
// connector's own format out of the wire contract.
func companionOptionalStamp(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return ""
	}
	return companionStamp(t)
}

// companionSourceKey narrows a connector key to the contract's opaque
// character set. A key that is empty after narrowing is dropped by the caller
// rather than replaced, because a made-up key attributes evidence to a source
// that did not produce it.
func companionSourceKey(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == '.' || r == ':':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return companionText(b.String(), companion.MaxSourceKeyBytes)
}

// companionOpaqueID derives a stable, bounded, opaque identifier from a vault
// stable id.
//
// It is a digest rather than the id itself for two reasons. A Mora stable id is
// "<kind>/<provider id>" — it carries the provider, often the account, and
// sometimes the message id, so shipping it puts provider identity on the phone
// and into anything that logs a URL. And it is unbounded, while the contract
// caps an identifier at 64 bytes with an opaque character set. The derivation is
// deterministic, so the same memory is the same id across requests and a client
// can deduplicate; it is one-way, so nothing downstream can parse a provider out
// of it.
func companionOpaqueID(prefix, stableID string) string {
	return prefix + companion.Fingerprint(stableID)[len("sha256:"):len("sha256:")+32]
}

// companionText truncates on a rune boundary so a clipped string is still valid
// UTF-8. Byte-slicing a multi-byte rune in half is how a bounded field becomes
// a replacement character in a phone's renderer.
func companionText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ---------------------------------------------------------------------------
// The publication record
// ---------------------------------------------------------------------------

// capturePublication is the durable record of one published capture, and it is
// the store's CANONICAL document.
//
// It is keyed by (device, idempotency key), because that is the question the
// record exists to answer: "has this key already published, and what did it
// answer with?". The reservation answers it too, but only until its crash window
// closes and the sweep collects it — so the answer has to live somewhere with a
// life of its own.
//
// It carries the response BYTES, so a replay after the reservation is gone is
// the same answer on the wire rather than a fresh receipt for the same memory.
// Round four kept the bytes only in the reservation, whose trim and TTL move
// independently of this store; an index that survived its reservation could
// therefore mint a second receipt for one publication.
//
// It lives under the STATE directory rather than beside the memory. The vault is
// the user's own tree — synced, opened in an editor, read by people — and a
// bookkeeping file with a digest in it does not belong there.
type capturePublication struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
	MemoryID      string `json:"memory_id"`
	DeviceID      string `json:"device_id"`
	Key           string `json:"idempotency_key"`
	Identity      string `json:"capture_identity"`
	ClaimedAt     string `json:"claimed_at"`
	// Response is the exact body the first attempt answered with. It is empty
	// between the claim and the receipt, which is precisely what "published but
	// not yet settled" looks like — and what makes the trim settled-aware
	// without the kernel having to see a reservation.
	Response string `json:"response,omitempty"`
}

const schemaCapturePublication = "mora.companion.capture.publication"

// capturePublicationIndex is the SECONDARY name: a pointer from a pinned memory
// id back to the canonical record.
//
// It is a pointer rather than a copy so the two can never disagree, and rather
// than a hard link because the canonical record is rewritten by rename when its
// receipt arrives — which would leave a link pointing at the version before the
// bytes existed.
type capturePublicationIndex struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
	MemoryID      string `json:"memory_id"`
	DeviceID      string `json:"device_id"`
	Key           string `json:"idempotency_key"`
}

const schemaCapturePublicationIndex = "mora.companion.capture.publication.index"

// matches reports whether this record describes the same capture.
//
// All three parts have to agree. The identity alone would let one device claim
// another's id if a digest ever collided; the device alone would let two captures
// from one phone share a memory.
func (p capturePublication) matches(id companion.CaptureIdentity) bool {
	return p.MemoryID == id.MemoryID &&
		p.DeviceID == id.DeviceID &&
		p.Key == id.Key &&
		p.Identity == id.Identity
}

// errMemoryIDMismatch reports that a pinned vault id is owned by a different
// capture. It is a vault-integrity failure, not a client error: nothing the
// phone sent was wrong.
var errMemoryIDMismatch = errors.New("mora: that memory id is claimed by a different capture")

// maxCapturePublicationBytes bounds one record on the read path, so a hostile or
// corrupt file is refused rather than allocated. It has room for a receipt.
const maxCapturePublicationBytes = 64 << 10

func capturePublicationDir(cfg Config) string {
	return filepath.Join(cfg.StateDir, "companion", "published")
}

// capturePublicationPath is the SECONDARY name: the pointer for one memory id.
//
// The id is validated by companion.CaptureIdentity before it reaches here and is
// restricted to the contract's opaque character set, so it carries no separator.
// filepath.Base is belt to that brace: a filename derived from a value that
// crosses a wire is never allowed to choose its own directory.
func capturePublicationPath(cfg Config, memoryID string) string {
	return filepath.Join(capturePublicationDir(cfg), filepath.Base(memoryID)+".json")
}

// capturePublicationKeyPath is the CANONICAL name: the record itself, found by
// the device and the idempotency key.
//
// The key is hashed rather than used as a filename, for the reason the
// reservation store hashes it: a name derived from a value that crossed a wire is
// never allowed to choose its own directory.
func capturePublicationKeyPath(cfg Config, deviceID, key string) string {
	digest := strings.TrimPrefix(companion.Fingerprint(key), "sha256:")
	return filepath.Join(capturePublicationDir(cfg), "keys", filepath.Base(deviceID), digest+".json")
}

// publicationClaim is what one claim created, and the only thing a failure after
// it may remove.
//
// Rollback works from this list rather than from the two names a capture COULD
// have created. The difference is the whole point: a claim that found the
// canonical record already there and only repaired the pointer must, if it later
// fails, take back the pointer and leave the record — and a claim that found
// somebody else's record must take back nothing at all.
type publicationClaim struct{ created []string }

// rollback removes exactly what this claim created, newest first.
func (c publicationClaim) rollback() error {
	var firstErr error
	for i := len(c.created) - 1; i >= 0; i-- {
		if err := os.Remove(c.created[i]); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// claimCapturePublication records that this capture owns the pinned id.
//
// # The order, and why it is that order
//
//	canonical (by key, temp + fsync + rename)  ->  pointer (by memory id)  ->  memory
//
// The canonical record is durable first, because it is the one a retry has to
// find. The pointer is derived from it and can always be rebuilt, so a crash
// between the two is repaired rather than fatal — every lookup consults the
// canonical record first and recreates a missing pointer when it sees one.
//
// The reverse order would be the broken one: a pointer with no record behind it
// names a memory nobody can prove ownership of.
//
// It reports the exact files it created, because a caller that later finds the
// memory foreign has to take back what it made and nothing else.
func claimCapturePublication(cfg Config, id companion.CaptureIdentity) (publicationClaim, error) {
	var claim publicationClaim
	keyPath := capturePublicationKeyPath(cfg, id.DeviceID, id.Key)
	idPath := capturePublicationPath(cfg, id.MemoryID)

	// The canonical record first, always. A key that already published something
	// is either this capture — in which case the pointer may need repairing — or
	// a different one, which is a refusal that touches nothing.
	existing, found, err := readCapturePublicationAt(keyPath)
	if err != nil {
		return claim, err
	}
	if found {
		if !existing.matches(id) {
			return claim, errMemoryIDMismatch
		}
		// The record is ours. Repair the pointer if the crash that brought us
		// here happened between the two writes.
		created, rerr := repairCapturePublicationIndex(cfg, id)
		if rerr != nil {
			return claim, rerr
		}
		if created {
			claim.created = append(claim.created, idPath)
		}
		return claim, nil
	}

	// No canonical record. A pointer that exists anyway belongs to somebody else's
	// publication, or is the debris of one — either way the id is not ours to take.
	if pointer, pfound, perr := readCapturePublicationIndexAt(idPath); perr != nil {
		return claim, perr
	} else if pfound && (pointer.DeviceID != id.DeviceID || pointer.Key != id.Key) {
		return claim, errMemoryIDMismatch
	}

	record := capturePublication{
		Schema:        schemaCapturePublication,
		SchemaVersion: 1,
		MemoryID:      id.MemoryID,
		DeviceID:      id.DeviceID,
		Key:           id.Key,
		Identity:      id.Identity,
		ClaimedAt:     mcpWriteClock().UTC().Truncate(time.Second).Format(time.RFC3339),
	}
	if err := writeCapturePublicationRecord(keyPath, record); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Somebody claimed the key between the read and the create. Whoever
			// they are, they are not us unless the record says so — and either
			// way this call created nothing.
			raced, rfound, rerr := readCapturePublicationAt(keyPath)
			if rerr != nil {
				return claim, rerr
			}
			if rfound && raced.matches(id) {
				return claim, nil
			}
			return claim, errMemoryIDMismatch
		}
		return claim, err
	}
	claim.created = append(claim.created, keyPath)

	created, err := repairCapturePublicationIndex(cfg, id)
	if err != nil {
		_ = claim.rollback()
		return publicationClaim{}, err
	}
	if created {
		claim.created = append(claim.created, idPath)
	}
	return claim, nil
}

// repairCapturePublicationIndex creates the pointer if it is missing, and
// reports whether it had to.
//
// A pointer that is already there and names this capture is left alone: it is
// not ours to have created, so it is not ours to roll back. A pointer that names
// a different capture is somebody else's id.
func repairCapturePublicationIndex(cfg Config, id companion.CaptureIdentity) (bool, error) {
	path := capturePublicationPath(cfg, id.MemoryID)
	pointer, found, err := readCapturePublicationIndexAt(path)
	if err != nil {
		return false, err
	}
	if found {
		if pointer.DeviceID != id.DeviceID || pointer.Key != id.Key {
			return false, errMemoryIDMismatch
		}
		return false, nil
	}
	body, err := json.MarshalIndent(capturePublicationIndex{
		Schema:        schemaCapturePublicationIndex,
		SchemaVersion: 1,
		MemoryID:      id.MemoryID,
		DeviceID:      id.DeviceID,
		Key:           id.Key,
	}, "", "  ")
	if err != nil {
		return false, err
	}
	if err := writeCapturePublicationExclusive(path, append(body, '\n')); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// recordCaptureReceipt stores the bytes a published capture answered with, into
// the canonical record.
//
// This is what makes a replay independent of the reservation. It is a rewrite
// through a temporary file and a rename, so a reader sees the record with the
// receipt or the record without it, never half of either.
func recordCaptureReceipt(cfg Config, id companion.CaptureIdentity, response []byte) error {
	keyPath := capturePublicationKeyPath(cfg, id.DeviceID, id.Key)
	record, found, err := readCapturePublicationAt(keyPath)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("companion capture: no publication record for %s to record a receipt against", id.MemoryID)
	}
	if !record.matches(id) {
		return errMemoryIDMismatch
	}
	if record.Response == string(response) {
		return nil
	}
	record.Response = string(response)
	return rewriteCapturePublicationRecord(keyPath, record)
}

// writeCapturePublicationRecord creates the canonical record, durably, and fails
// if it is already there.
//
// The bytes and the directory entry are both synced before it returns: this
// record is the proof a later retry reads, and a proof that can evaporate in a
// power cut is not one.
func writeCapturePublicationRecord(path string, record capturePublication) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return publishCapturePublicationRecord(path, append(body, '\n'))
}

// rewriteCapturePublicationRecord replaces an existing canonical record.
func rewriteCapturePublicationRecord(path string, record capturePublication) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return publishCapturePublicationRecord(path, append(body, '\n'))
}

// publishCapturePublicationRecord stages the bytes beside the record and renames
// them into place, syncing both.
func publishCapturePublicationRecord(path string, body []byte) error {
	if len(body) > maxCapturePublicationBytes {
		return fmt.Errorf("companion capture: a publication record of %d bytes is over the %d-byte limit", len(body), maxCapturePublicationBytes)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".pub-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return atomicio.SyncDir(dir)
}

// writeCapturePublicationExclusive creates one small file, and fails if it is
// already there. It is the pointer's write: derived, rebuildable, and not worth
// an fsync of its own.
func writeCapturePublicationExclusive(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// publishedForKey answers "did this key already publish something, and what?".
//
// It is the lookup a FRESH reservation makes before it claims anything, and it
// reads the canonical record — the reservation store cannot answer it, because a
// capture killed after its publication leaves a pending row the sweep collects
// and after that the key looks unused.
func publishedForKey(cfg Config, deviceID, key string) (capturePublication, bool, error) {
	return readCapturePublicationAt(capturePublicationKeyPath(cfg, deviceID, key))
}

// readCapturePublication finds a record by the pinned memory id, through the
// pointer, and repairs a pointer that has gone missing.
//
// It consults the canonical record in every case: a pointer is a shortcut, never
// an authority.
func readCapturePublication(cfg Config, memoryID string) (capturePublication, bool, error) {
	pointer, found, err := readCapturePublicationIndexAt(capturePublicationPath(cfg, memoryID))
	if err != nil || !found {
		return capturePublication{}, false, err
	}
	record, found, err := readCapturePublicationAt(capturePublicationKeyPath(cfg, pointer.DeviceID, pointer.Key))
	if err != nil || !found {
		return capturePublication{}, false, err
	}
	if record.MemoryID != memoryID {
		return capturePublication{}, false, nil
	}
	return record, true, nil
}

// readCapturePublicationAt reads one canonical record.
func readCapturePublicationAt(path string) (capturePublication, bool, error) {
	body, found, err := readCapturePublicationBytes(path)
	if err != nil || !found {
		return capturePublication{}, false, err
	}
	var record capturePublication
	if err := json.Unmarshal(body, &record); err != nil {
		return capturePublication{}, false, fmt.Errorf("companion capture: %s is not readable as a publication record: %w", path, err)
	}
	if record.Schema != schemaCapturePublication {
		return capturePublication{}, false, fmt.Errorf("companion capture: %s is not a publication record", path)
	}
	return record, true, nil
}

// readCapturePublicationIndexAt reads one pointer.
func readCapturePublicationIndexAt(path string) (capturePublicationIndex, bool, error) {
	body, found, err := readCapturePublicationBytes(path)
	if err != nil || !found {
		return capturePublicationIndex{}, false, err
	}
	var pointer capturePublicationIndex
	if err := json.Unmarshal(body, &pointer); err != nil {
		return capturePublicationIndex{}, false, fmt.Errorf("companion capture: %s is not readable as a publication index: %w", path, err)
	}
	if pointer.Schema != schemaCapturePublicationIndex {
		return capturePublicationIndex{}, false, fmt.Errorf("companion capture: %s is not a publication index", path)
	}
	return pointer, true, nil
}

// readCapturePublicationBytes reads one bounded file. A missing one is not an
// error: the state directory can be rebuilt independently of the vault.
func readCapturePublicationBytes(path string) ([]byte, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxCapturePublicationBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > maxCapturePublicationBytes {
		return nil, false, fmt.Errorf("companion capture: %s is over the %d-byte limit", path, maxCapturePublicationBytes)
	}
	return body, true, nil
}

// removeCapturePublication takes back both names of one publication. It is used
// only by the trim, which owns every record it evicts.
func removeCapturePublication(cfg Config, record capturePublication) {
	_ = os.Remove(capturePublicationPath(cfg, record.MemoryID))
	_ = os.Remove(capturePublicationKeyPath(cfg, record.DeviceID, record.Key))
}

// ---------------------------------------------------------------------------
// The published-store census
// ---------------------------------------------------------------------------

// publishedCensus keeps the published store inside its bound without walking it.
//
// It is the same shape the reservation store uses and for the same reason: a
// count taken by reading every record on every claim makes the Nth capture pay
// for the N-1 before it. One walk seeds it, and the trim moves it.
//
// It is deliberately seeded at TRIM time rather than at claim time, so a fresh
// claim walks nothing at all — the trim runs only after a capture has settled,
// which is the one moment a walk is what the caller is asking for.
type publishedCensus struct {
	mu      sync.Mutex
	seeded  bool
	records []capturePublication
	// readDir is the walk, a field so a test can COUNT it. The claim it exists
	// for is a cost claim, and a cost claim needs something countable.
	readDir func(name string) ([]os.DirEntry, error)
}

func newPublishedCensus() *publishedCensus { return &publishedCensus{readDir: os.ReadDir} }

// note records a publication the census has not seen, without a walk.
func (c *publishedCensus) note(record capturePublication) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.seeded {
		return
	}
	for i, existing := range c.records {
		if existing.MemoryID == record.MemoryID {
			c.records[i] = record
			return
		}
	}
	c.records = append(c.records, record)
}

// trim keeps the store inside the same total cap the reservation store uses,
// dropping the oldest SETTLED records first.
//
// Settled-aware means a record with no response bytes is never evicted: those
// bytes arrive when the capture settles, so their absence is exactly "this
// publication is still in flight", and evicting one would take away the proof a
// retry is about to need.
//
// It runs after a capture has settled, never on a claim, so a REJECTED request
// cannot evict anything — which is what makes "the published tree is unchanged"
// true of a refusal.
func (c *publishedCensus) trim(cfg Config, keep companion.CaptureIdentity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.seeded {
		c.records = c.walk(cfg)
		c.seeded = true
	}
	if len(c.records) <= companion.MaxReservations {
		return
	}
	sort.SliceStable(c.records, func(i, j int) bool { return c.records[i].ClaimedAt < c.records[j].ClaimedAt })
	excess := len(c.records) - companion.MaxReservations
	survivors := make([]capturePublication, 0, len(c.records))
	for _, record := range c.records {
		if excess > 0 && record.MemoryID != keep.MemoryID && record.Response != "" {
			removeCapturePublication(cfg, record)
			excess--
			continue
		}
		survivors = append(survivors, record)
	}
	c.records = survivors
}

// walk reads the canonical records. It is the one directory pass in the census's
// life, and it walks the CANONICAL tree — the pointers are derived and would
// double the count.
func (c *publishedCensus) walk(cfg Config) []capturePublication {
	root := filepath.Join(capturePublicationDir(cfg), "keys")
	devices, err := c.readDir(root)
	if err != nil {
		return nil
	}
	out := []capturePublication{}
	for _, device := range devices {
		if !device.IsDir() {
			continue
		}
		entries, err := c.readDir(filepath.Join(root, device.Name()))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			record, found, rerr := readCapturePublicationAt(filepath.Join(root, device.Name(), entry.Name()))
			if rerr != nil || !found {
				continue
			}
			out = append(out, record)
		}
	}
	return out
}

// companionAlreadyPublished reports whether the governed write path found the
// pinned id already in the vault rather than creating it.
func companionAlreadyPublished(result any) bool {
	wrapped, ok := result.(map[string]any)
	if !ok {
		return false
	}
	already, _ := wrapped["already_published"].(bool)
	return already
}

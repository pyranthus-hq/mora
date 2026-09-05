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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	"github.com/pyranthus-hq/mora/internal/leasefile"
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
	allowHost := fs.String("allow-host", "", "one exact Host value to accept from a loopback reverse proxy (see `mora companion expose`); empty keeps loopback-only")
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
		Addr:      fmt.Sprintf("%s:%d", companion.LoopbackHost, *port),
		AllowHost: *allowHost,
		Devices:   reg,
		Pairings:  reg,
		Reader:    newCompanionReader(cfg),
		Writer:    newCompanionWriter(),
		Captures:  companion.NewReservationStore(cfg.StateDir, companion.WithReservationClock(cfg.OperationClock)),
		Now:       cfg.OperationClock,
		Log:       stdout,
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
// It is called after the capture has been APPLIED and before its reservation
// settles — the receipt bytes have to be durable in the published store first,
// because the other order left a record with no bytes behind any crash in
// between. Two things follow, and they are what the trim relies on: a rejected
// request never reaches this function at all, so a refusal leaves the store
// byte-identical, and the trim never evicts a publication whose bytes have not
// arrived, because a record with no response bytes is a publication still in
// flight and the trim skips those.
func (w *companionWriter) RecordReceipt(ctx context.Context, id companion.CaptureIdentity, response []byte) ([]byte, error) {
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return nil, err
	}
	recorded, err := recordCaptureReceipt(cfg, id, response)
	if err != nil {
		return nil, companionIntegrityError(err)
	}
	record, found, err := readCapturePublicationAt(capturePublicationKeyPath(cfg, id.DeviceID, id.Key))
	if err != nil {
		return nil, companionIntegrityError(err)
	}
	if found {
		record.Response = string(recorded)
		w.census.note(record)
	}
	w.census.trim(cfg, id)
	// The AUTHORITATIVE bytes go back, which are the sibling's whether this call
	// wrote it or lost the race for it. A caller that answered with its own
	// locally built receipt would give two racing claimants two different
	// receipts for one publication.
	return recorded, nil
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
		return companion.WriteOutcome{}, false, companionIntegrityError(err)
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
		// Including a pointer that disagrees with its record: an id whose
		// bookkeeping contradicts itself is never this capture's.
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

// companionIntegrityError maps the kernel's own mismatch sentinel onto the one
// the companion capture path understands.
//
// errMemoryIDMismatch and companion.ErrPublishedIntegrity name the same
// condition from two sides of the seam: bookkeeping in the published store that
// contradicts itself, with nothing wrong in what the phone sent. The companion
// side keys on ErrPublishedIntegrity to settle a `rejected: internal` receipt
// with the cause on the outcome; an unmapped mismatch fell through as a plain
// error, so the request became a 503 and the operator was told nothing at all —
// the same vault fault answered two different ways depending on which lookup
// happened to find it first.
//
// Both sentinels stay in the chain. Callers inside this package that already
// branch on errMemoryIDMismatch keep working, and the companion side gets the
// contract error it tests for.
func companionIntegrityError(err error) error {
	if err == nil || !errors.Is(err, errMemoryIDMismatch) {
		return err
	}
	return fmt.Errorf("%w: %w", companion.ErrPublishedIntegrity, err)
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
		return "", false, nil, companionIntegrityError(err)
	}
	// A replay repairs the pointer on its way past. The canonical record is the
	// authority, so a missing pointer is not an error — but leaving it missing
	// would mean the by-id lookup keeps answering "absent" for a memory that is
	// very much published.
	// A pointer that disagrees with the canonical record is an integrity failure,
	// not a thing to shrug at: it is reported so the capture path settles a
	// rejection rather than proceeding over contradictory bookkeeping.
	if _, rerr := repairCapturePublicationIndex(cfg, companion.CaptureIdentity{
		DeviceID: record.DeviceID, Key: record.Key, MemoryID: record.MemoryID, Identity: record.Identity,
	}); rerr != nil {
		return "", false, nil, companionIntegrityError(rerr)
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
//	canonical (by key, temp + fsync + LINK)  ->  pointer (by memory id)  ->  memory
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
	if err := writeCapturePublicationRecord(capturePublicationLockDir(cfg), keyPath, record); err != nil {
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

// capturePublicationRaceGate is a no-op seam that runs at the instant two
// concurrent claims for one key would collide.
//
// It exists so the exclusivity below can be WITNESSED rather than asserted: a
// race is only a race if two callers can be held at the same point, and holding
// them is the only way to make the test deterministic instead of hopeful.
// Production replaces it with nothing.
var capturePublicationRaceGate = func() {}

// writeCapturePublicationRecord creates the canonical record, durably and
// EXCLUSIVELY, and fails with os.ErrExist if somebody else already owns it.
//
// # Why a link rather than a stat and a rename
//
// Stat-then-rename is not exclusive and the gap is the whole defect: two claims
// for one key both see "absent", both rename their own staging file over the
// path, and the loser then believes it created a record that is actually the
// winner's — so its rollback deletes the winner's publication. os.Link is the
// primitive that decides: the filesystem refuses the second link with EEXIST, so
// exactly one caller ever learns it created the file, and the other learns it
// did not create anything and must roll back nothing.
//
// The staging file carries the bytes and is fsynced BEFORE the link, so the
// record is never visible half-written and never visible without being durable.
//
// On Windows, os.Link is a hard link on NTFS and behaves the same way. A
// filesystem that does not support links at all falls back to an O_EXCL create
// of the final path — still exclusive, at the cost of a window in which a reader
// could see a short file, which readCapturePublicationAt reports as unreadable
// rather than as absent. That is the safe direction: a record that cannot be
// read is not a record that says the key is free.
func writeCapturePublicationRecord(locks, path string, record capturePublication) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	capturePublicationRaceGate()
	return linkCapturePublicationRecord(locks, path, append(body, '\n'))
}

// capturePublicationLink is the exclusive primitive, a seam so a test can make
// it fail in the ways a filesystem does. Production is os.Link.
var capturePublicationLink = os.Link

// capturePublicationWrite is the staging write, a seam for the same reason: the
// branch that matters is the one where a record cannot be finished, and a file
// that cannot be finished must never appear under its final name.
var capturePublicationWrite = func(file *os.File, body []byte) (int, error) { return file.Write(body) }

// linkCapturePublicationRecord stages the bytes, syncs them, and links them into
// place under a name nobody else holds.
func linkCapturePublicationRecord(locks, path string, body []byte) error {
	if len(body) > maxCapturePublicationBytes {
		return fmt.Errorf("companion capture: a publication record of %d bytes is over the %d-byte limit", len(body), maxCapturePublicationBytes)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name, err := stageCapturePublicationBytes(dir, body)
	if err != nil {
		return err
	}
	defer os.Remove(name)

	err = capturePublicationLink(name, path)
	switch {
	case err == nil:
		return atomicio.SyncDir(dir)
	case errors.Is(err, os.ErrExist):
		// Somebody else owns this name. Nothing here created anything, which is
		// exactly what the caller has to know.
		return os.ErrExist
	case !linkUnsupported(err):
		// A real fault, not a filesystem that cannot do links. Round six fell
		// back on EVERY non-EEXIST error, which turned an I/O fault into a direct
		// write to the final path — masking the fault and risking a half-written
		// canonical record if the process died mid-write. EXDEV is deliberately
		// not in the unsupported set: the staging file is created in the
		// destination's own directory, so a cross-device link is a bug, not a
		// limitation.
		// A real fault. It is reported, and nothing is left behind: the staging
		// file goes with the defer and the destination was never touched.
		return err
	}
	return claimAndPublishCapturePublication(locks, path, name, dir)
}

// claimAndPublishCapturePublication is the no-links fallback.
//
// It is still the filesystem deciding, and it still never exposes a partial
// file. The ownership token is an O_EXCL create of "<path>.claim" — that is the
// exclusive step, and whoever wins it is the only caller that then renames the
// already-fsynced staging file onto the final name. Writing directly into the
// final path, which is what round seven did, left a window in which a losing
// reader could see an empty or half-written record and take it for the answer.
//
// # The token is reclaimable, because a bare token wedges the name forever
//
// Round eight left the token as a plain empty file removed on the way out. A
// process killed between the create and the rename orphaned it, and from then on
// every claimant for that (device, key) got EEXIST from a token whose owner was
// never coming back — the canonical publication for that key was wedged for the
// life of the state directory, and the receipt-repair path recursed on it.
//
// So the token carries WHO holds it and WHEN they took it, and a claimant that
// finds a corpse — a dead pid, or a token older than companion.ReservationTakeover,
// the same window a crashed reservation is taken over in — reclaims it. A token
// whose owner is alive and inside the window is never removed: the claimant
// reports errCapturePublicationBusy and the phone retries.
//
// The reclaim itself is exclusive, and NOT by a second O_EXCL sentinel. That was
// N11's lesson and it applies here unchanged: any staleness rule enforced with
// O_EXCL is a check-then-use race, because between "this token looks stale" and
// "I removed it" a second reclaimer can be at exactly the same point. The whole
// publish — take the token, verify, rename, release — therefore runs under a
// kernel-held lock (internal/leasefile, flock on POSIX and a LockFileEx range on
// Windows), which has no staleness rule of its own because the kernel drops it
// when the holder dies.
//
// # ABA, and why the lock is not the only defence
//
// Round nine held the lock only across the token REPLACEMENT and then let go.
// That left an ABA: an owner whose token had aged past the takeover window but
// whose process was merely slow could resume, pass the pre-rename stat, rename
// after it had already lost ownership, and then unconditionally remove its
// SUCCESSOR's token — so two claimants could both pass the stat and one could
// overwrite or delete the other's canonical publication.
//
// Widening the lock closes it between cooperating processes. It is not enough on
// its own, because a lock is only as good as the thing enforcing it: an flock is
// advisory and follows the INODE, so an external `rm` of the guard file hands two
// callers two different locks, and there are network filesystems where it does
// not exclude at all. So the token carries a per-claim NONCE, and every step that
// acts on it — the rename and the removal both — re-reads the token and compares
// the nonce IMMEDIATELY before acting. A claimant that has lost ownership renames
// nothing, deletes nothing, and reports the retryable busy error.
func claimAndPublishCapturePublication(locks, path, staged, dir string) error {
	token := path + ".claim"
	return withCapturePublicationGuard(locks, token, func() error {
		nonce, err := takeCaptureClaimToken(token)
		if err != nil {
			return err
		}
		return publishUnderCaptureClaimToken(token, nonce, path, staged, dir)
	})
}

// publishUnderCaptureClaimToken renames the staged record into place, and every
// step it takes is guarded by the nonce it was handed.
//
// The ordering is the point:
//
//	verify -> stat -> verify -> RENAME -> sync -> verify -> remove token
//
// The verify before the rename is the one the ABA slipped through: the stat is a
// check, the rename is the use, and round nine had nothing between them. The
// verify before the removal is the other half — it is what stops a claimant that
// has lost ownership from deleting the token its successor is publishing under.
func publishUnderCaptureClaimToken(token, nonce, path, staged, dir string) error {
	if err := verifyCaptureClaimToken(token, nonce); err != nil {
		// Ownership is already gone. Nothing here may touch the token or the
		// name: both belong to whoever holds the token now.
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return releaseCaptureClaimToken(token, nonce, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return releaseCaptureClaimToken(token, nonce, err)
	}

	// The seam sits between the stat and the rename because that is the window
	// the ABA lives in. Production replaces it with nothing.
	captureClaimRenameGate()

	if err := verifyCaptureClaimToken(token, nonce); err != nil {
		return err
	}
	if err := os.Rename(staged, path); err != nil {
		return releaseCaptureClaimToken(token, nonce, err)
	}
	if err := atomicio.SyncDir(dir); err != nil {
		_ = os.Remove(path)
		return releaseCaptureClaimToken(token, nonce, err)
	}
	return releaseCaptureClaimToken(token, nonce, nil)
}

// captureClaimRenameGate is a no-op seam that runs in the window between the
// pre-rename stat and the rename itself.
var captureClaimRenameGate = func() {}

// verifyCaptureClaimToken reports whether the token on disk is still the one
// this caller took.
//
// A missing token, an unreadable one, or one carrying somebody else's nonce all
// mean the same thing: this caller no longer owns the name. It is the retryable
// busy error rather than a fault, because the capture itself is fine — the
// publication is being made by somebody else right now, and the retry will find
// their record and replay it.
func verifyCaptureClaimToken(token, nonce string) error {
	held, found, err := readCaptureClaimToken(token)
	if err != nil {
		return err
	}
	if !found || held.Nonce == "" || held.Nonce != nonce {
		return errCapturePublicationBusy
	}
	return nil
}

// releaseCaptureClaimToken removes the token if and only if it is still ours,
// and returns cause unchanged.
//
// The nonce check is not a tidiness measure. Round nine's release was a bare
// os.Remove, so a claimant that had lost ownership deleted the token its
// successor was publishing under — and the name then looked free to a third
// claimant while a rename was still in flight against it.
//
// A token that is no longer ours is left exactly where it is and is NOT reported
// as an error on a successful publish: the record is renamed into place and
// durable at that point, and the next claimant reads the record before it ever
// looks at a token.
func releaseCaptureClaimToken(token, nonce string, cause error) error {
	held, found, err := readCaptureClaimToken(token)
	if err != nil {
		if cause != nil {
			return cause
		}
		return err
	}
	if !found || held.Nonce != nonce {
		return cause
	}
	if err := os.Remove(token); err != nil && !errors.Is(err, os.ErrNotExist) {
		if cause != nil {
			return cause
		}
		return err
	}
	return cause
}

// captureClaimToken is what the fallback writes into its ownership token so a
// later claimant can tell a live publisher from a corpse, and so the holder can
// tell whether it is still the holder.
//
// The pid is only meaningful on the host that wrote it, which is the only host
// that ever reads it: the published store lives under this Mac's own state
// directory. Pid REUSE is the case the timestamp covers — a recycled pid makes a
// dead owner look alive, and the takeover window then retires the token anyway.
//
// The NONCE is per claim rather than per process, and that is what pid and
// timestamp cannot do. One process can take, lose and retake the same token, and
// two claims from one pid inside one second are indistinguishable by either of
// the other two fields — so ownership is identified by the nonce and nothing
// else.
type captureClaimToken struct {
	Schema  string `json:"schema"`
	PID     int    `json:"pid"`
	Nonce   string `json:"nonce"`
	TakenAt string `json:"taken_at"`
}

const schemaCaptureClaimToken = "mora.companion.capture.publication.claim"

// errCapturePublicationBusy reports that another attempt holds the fallback's
// ownership token for this name right now, or that this attempt has lost it.
//
// It is deliberately NOT errMemoryIDMismatch. A contended token says nothing
// about who owns the id — it is the same key's own concurrent attempt or its
// takeover — and reporting it as an integrity failure would settle a permanent
// rejection over a capture that is merely in flight. A retryable error is the
// honest answer: the phone sees 503 and the retry finds the record and replays
// it.
var errCapturePublicationBusy = errors.New("mora: another attempt is publishing that capture record right now")

// takeCaptureClaimToken wins the exclusive right to rename onto the record's
// name, reclaiming a corpse if it finds one, and returns the nonce that proves
// the claim.
//
// Its caller holds the guard across this AND the publish that follows, so no two
// callers are ever between their own read and their own create — and no caller
// is ever between its own create and its own rename while another takes the
// token away.
func takeCaptureClaimToken(token string) (string, error) {
	nonce, err := createCaptureClaimToken(token)
	if err == nil {
		return nonce, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	stale, serr := captureClaimTokenIsStale(token, mcpWriteClock())
	if serr != nil {
		return "", serr
	}
	if !stale {
		// A live publisher. It is not removed, not raced, and not waited on
		// inside a request: this attempt reports busy and the phone retries.
		return "", errCapturePublicationBusy
	}
	if rmErr := os.Remove(token); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return "", rmErr
	}
	return createCaptureClaimToken(token)
}

// createCaptureClaimToken writes one complete token, exclusively, and reports
// os.ErrExist if somebody already holds the name. It returns the nonce that
// identifies this claim.
//
// The bytes are written before the guard is released, which is what lets a later
// claimant treat an UNREADABLE token as a corpse rather than as a live claim it
// caught mid-write.
func createCaptureClaimToken(token string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(token), 0o700); err != nil {
		return "", err
	}
	nonce, err := captureClaimNonce()
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(token, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(captureClaimToken{
		Schema:  schemaCaptureClaimToken,
		PID:     os.Getpid(),
		Nonce:   nonce,
		TakenAt: mcpWriteClock().UTC().Truncate(time.Second).Format(time.RFC3339),
	})
	if err == nil {
		_, err = file.Write(append(body, '\n'))
	}
	if err == nil {
		err = file.Close()
	} else {
		_ = file.Close()
	}
	if err != nil {
		_ = os.Remove(token)
		return "", err
	}
	return nonce, nil
}

// captureClaimNonce mints the value that identifies one claim.
//
// It FAILS rather than degrading when the CSPRNG does. Elsewhere in this package
// a random suffix is a uniqueness token and a PRNG fallback is the right call;
// here the nonce is the whole ownership proof, and a predictable or repeated one
// would let a second claimant's verify pass against the first claimant's token.
// Refusing the publish is the safe direction: the caller reports an error, the
// phone retries, and nothing is renamed on a claim nobody can prove.
func captureClaimNonce() (string, error) {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		return "", fmt.Errorf("companion capture: no entropy for a publication claim: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// readCaptureClaimToken reads one ownership token. A missing or unreadable one
// is "not found" rather than an error: the callers all treat "this is not a
// token that names me" the same way.
func readCaptureClaimToken(token string) (captureClaimToken, bool, error) {
	body, found, err := readCapturePublicationBytes(token)
	if err != nil || !found {
		return captureClaimToken{}, false, err
	}
	var held captureClaimToken
	if json.Unmarshal(body, &held) != nil || held.Schema != schemaCaptureClaimToken {
		return captureClaimToken{}, false, nil
	}
	return held, true, nil
}

// captureClaimTokenIsStale reports whether the token on disk is a corpse.
//
// Three ways to be one, and each is a different failure:
//
//   - the owner's process is gone, which is the common crash and is retired at
//     once rather than after a window;
//   - the token is older than companion.ReservationTakeover, which covers a pid
//     the operating system has recycled onto some unrelated live program;
//   - the token cannot be read as a token at all AND its mtime is past the same
//     window. An unreadable token is either debris from a build that wrote empty
//     ones or a truncated file; either way it is only retired once it is old,
//     so a token this process is at that instant writing is never mistaken for
//     one.
func captureClaimTokenIsStale(token string, now time.Time) (bool, error) {
	info, err := os.Stat(token)
	if errors.Is(err, os.ErrNotExist) {
		// It went away between the failed create and this read. Whoever removed
		// it left the name free, and the create below decides.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	held, found, err := readCaptureClaimToken(token)
	if err != nil || !found {
		return !now.Before(info.ModTime().Add(companion.ReservationTakeover)), nil
	}
	if held.PID > 0 && !captureProcessAlive(held.PID) {
		return true, nil
	}
	takenAt, perr := time.Parse(time.RFC3339, held.TakenAt)
	if perr != nil {
		return !now.Before(info.ModTime().Add(companion.ReservationTakeover)), nil
	}
	return !now.Before(takenAt.Add(companion.ReservationTakeover)), nil
}

// captureProcessAlive reports whether a pid still names a running process, and
// is a seam so a test can present a corpse without killing anything.
//
// It is internal/operation's liveness probe, unchanged: the build-tagged
// signal-zero probe on POSIX and an OpenProcess on Windows. Reusing it rather
// than writing a second one keeps one definition of "that process is gone" in
// the product.
//
// Its POSIX half answers false for a process this user cannot signal. That is
// the wrong direction in general and the right one here: the published store
// lives in a 0700 directory under this user's own home, so every token in it was
// written by this user, and a pid that cannot be signalled is a pid that has
// been recycled onto somebody else's program — a corpse by another name. The
// takeover window covers the case either way.
var captureProcessAlive = processAlive

// capturePublicationLockDir is where the published store's cross-process guards
// live.
//
// It is a SIBLING of the published tree rather than a directory inside it, and
// that is load-bearing rather than tidy: a refusal must leave the published tree
// byte for byte as it found it, and a guard file created inside it would be a
// byte of difference. Guards also outlive every process by design — nothing ever
// removes them — so a tree that had to stay clean could not hold them.
func capturePublicationLockDir(cfg Config) string {
	return filepath.Join(cfg.StateDir, "companion", "publocks")
}

// withCapturePublicationGuard runs fn holding the kernel lock for one name.
//
// The key is hashed so the guard is one flat file per protected name whatever
// the name's own depth, and internal/leasefile owns the platform split — flock
// on POSIX, a LockFileEx byte range on Windows — plus the rule that matters
// most here: the lock file is never removed, so there is no staleness protocol
// and no window in which two holders exist.
func withCapturePublicationGuard(locks, key string, fn func() error) error {
	sum := sha256.Sum256([]byte(key))
	return leasefile.WithGuard(filepath.Join(locks, hex.EncodeToString(sum[:])+".lock"), fn)
}

// stageCapturePublicationBytes writes the record beside its destination and
// makes it durable before anybody can link to it.
func stageCapturePublicationBytes(dir string, body []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, ".pub-*.tmp")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if _, err := capturePublicationWrite(tmp, body); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// capturePublicationReceiptPath is the receipt SIBLING of one canonical record.
//
// The receipt lives beside the record rather than inside it because the record is
// IMMUTABLE once linked. Round six rewrote the record in place to add the bytes,
// through an unguarded rename — last writer wins, so two callers that both
// reached the receipt could mint different bodies for one publication. A sibling
// claimed with the same exclusive primitive makes the receipt as decided as the
// record: the first one to link it wins, and everybody else reads what it wrote.
func capturePublicationReceiptPath(cfg Config, deviceID, key string) string {
	return capturePublicationKeyPath(cfg, deviceID, key) + ".receipt"
}

// recordCaptureReceipt claims the receipt sibling, exclusively, and returns the
// bytes that ended up there.
//
// EEXIST is not a failure: somebody else recorded the receipt for this
// publication first, and theirs is the answer. Returning it rather than
// overwriting is what makes one publication have one receipt no matter how many
// callers reach this point.
//
// # One repair, then a typed error, and never recursion
//
// The name can be taken by something that is not a usable receipt — a torn
// fallback write, debris somebody left behind — and that has to be repaired
// rather than answered with. Round eight repaired it by removing the file and
// CALLING ITSELF, which is unbounded: a state that keeps coming back torn (a
// read-only sibling, a directory at the name, a filesystem returning stale
// bytes) recurses until the stack runs out. So the repair happens exactly once,
// there is one attempt after it, and a sibling still unusable at that point is
// errCaptureReceiptUnrepaired.
func recordCaptureReceipt(cfg Config, id companion.CaptureIdentity, response []byte) ([]byte, error) {
	record, found, err := readCapturePublicationAt(capturePublicationKeyPath(cfg, id.DeviceID, id.Key))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("companion capture: no publication record for %s to record a receipt against", id.MemoryID)
	}
	if !record.matches(id) {
		return nil, errMemoryIDMismatch
	}
	locks := capturePublicationLockDir(cfg)
	path := capturePublicationReceiptPath(cfg, id.DeviceID, id.Key)

	body, done, err := recordCaptureReceiptOnce(locks, path, id, response)
	if err != nil {
		return nil, err
	}
	if done {
		return body, nil
	}
	captureReceiptRepairGate()
	body, done, err = repairCaptureReceiptSibling(locks, path, id, response)
	if err != nil {
		return nil, err
	}
	if done {
		return body, nil
	}
	return nil, fmt.Errorf("%w: %s", errCaptureReceiptUnrepaired, path)
}

// errCaptureReceiptUnrepaired reports a receipt sibling that is neither usable
// nor replaceable. It is terminal for the attempt and deliberately not a
// rejection: the publication is real, and a receipt that cannot be written is a
// fault to retry, not a verdict on the capture.
var errCaptureReceiptUnrepaired = errors.New("mora: the receipt sibling could not be recorded or repaired")

// captureReceiptRepairGate is a no-op seam that runs at the instant two callers
// have BOTH decided the sibling needs repairing and neither has acted.
//
// That is the collision point, and holding both there is the only way to make
// the two-repairer race deterministic rather than hopeful. Production replaces
// it with nothing.
var captureReceiptRepairGate = func() {}

// recordCaptureReceiptOnce links the bytes, or reads back the whole sibling
// somebody else linked.
//
// done=false with no error is the one interesting answer: the name is taken by
// something that is not a usable receipt, which is the state the repair exists
// for.
func recordCaptureReceiptOnce(locks, path string, id companion.CaptureIdentity, response []byte) ([]byte, bool, error) {
	err := linkCapturePublicationRecord(locks, path, response)
	if err == nil {
		return response, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}
	theirs, found, rerr := readCaptureReceipt(path, id)
	if rerr != nil {
		return nil, false, rerr
	}
	return theirs, found, nil
}

// repairCaptureReceiptSibling replaces a torn sibling, and does it through the
// SAME kind of exclusion the first publication used.
//
// # Why the exclusion is the whole fix
//
// Round eight's repair was remove-then-write with nothing holding the name in
// between. Two repairers that both found the sibling torn both removed it and
// both wrote, so one of them deleted a receipt the other had already published
// and the two attempts answered the same publication with different bytes —
// which is precisely the property the sibling exists to guarantee against.
//
// Under the guard exactly one repairer acts. The other arrives after it, finds a
// WHOLE sibling, and answers with those bytes: it deletes nothing, writes
// nothing, and returns byte-for-byte what the winner returned. The guard is
// keyed on ".repair" rather than on the sibling's own name so it can never
// collide with the fallback's token guard for the same path, which this function
// takes underneath it.
func repairCaptureReceiptSibling(locks, path string, id companion.CaptureIdentity, response []byte) ([]byte, bool, error) {
	var (
		body []byte
		done bool
	)
	err := withCapturePublicationGuard(locks, path+".repair", func() error {
		// Re-read under the guard. A sibling that has become whole since the
		// caller looked is the winner's, and answering with it is what stops a
		// valid receipt from being deleted by a repairer that was one step
		// behind.
		theirs, found, rerr := readCaptureReceipt(path, id)
		if rerr != nil {
			return rerr
		}
		if found {
			body, done = theirs, true
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return rmErr
		}
		lerr := linkCapturePublicationRecord(locks, path, response)
		if lerr == nil {
			body, done = response, true
			return nil
		}
		if errors.Is(lerr, os.ErrExist) || errors.Is(lerr, errCapturePublicationBusy) {
			// The name came back under this guard, which means something outside
			// the published store is writing it. It is not repaired again here:
			// the caller reports the typed error rather than looping.
			return nil
		}
		return lerr
	})
	return body, done, err
}

// readCaptureReceipt reads one receipt sibling, and reports it only if it is a
// WHOLE one.
//
// Absent, empty, undecodable, or describing another capture all mean the same
// thing to a caller: "published, receipt not yet recorded". That is the state a
// retry completes, so a sibling that cannot be trusted is repaired rather than
// answered with — which is what keeps a torn or corrupt file from becoming the
// permanent answer to every future replay.
func readCaptureReceipt(path string, id companion.CaptureIdentity) ([]byte, bool, error) {
	body, found, err := readCapturePublicationBytes(path)
	if err != nil || !found || len(body) == 0 {
		return nil, false, err
	}
	var receipt companion.Receipt
	if json.Unmarshal(body, &receipt) != nil {
		return nil, false, nil
	}
	if receipt.Validate() != nil {
		return nil, false, nil
	}
	if receipt.IdempotencyKey != id.Key || receipt.DeviceID != id.DeviceID {
		return nil, false, nil
	}
	return body, true, nil
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
	record, found, err := readCapturePublicationAt(capturePublicationKeyPath(cfg, deviceID, key))
	if err != nil || !found {
		return record, found, err
	}
	// The record is the claim; the sibling is the answer. A record without one is
	// "published, receipt not yet recorded", and the empty Response is what tells
	// the caller to record it.
	response, hasReceipt, err := readCaptureReceipt(capturePublicationReceiptPath(cfg, deviceID, key), companion.CaptureIdentity{
		DeviceID: record.DeviceID, Key: record.Key, MemoryID: record.MemoryID, Identity: record.Identity,
	})
	if err != nil {
		return capturePublication{}, false, err
	}
	if hasReceipt {
		record.Response = string(response)
	}
	return record, true, nil
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
		// The pointer names a record about a different memory. Answering "absent"
		// would send a retry into a write at an id somebody else's publication
		// already owns; answering with the record would hand back a receipt for a
		// memory nobody asked about. It is a vault-integrity failure, and it is
		// reported as one.
		return capturePublication{}, false, fmt.Errorf("%w: the pointer for %s names %s",
			companion.ErrPublishedIntegrity, memoryID, record.MemoryID)
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
	_ = os.Remove(capturePublicationReceiptPath(cfg, record.DeviceID, record.Key))
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
			// Settled-awareness reads the sibling: a record with no receipt is a
			// publication still in flight, and the trim never takes one.
			if response, hasReceipt, _ := readCaptureReceipt(capturePublicationReceiptPath(cfg, record.DeviceID, record.Key), companion.CaptureIdentity{
				DeviceID: record.DeviceID, Key: record.Key, MemoryID: record.MemoryID, Identity: record.Identity,
			}); hasReceipt {
				record.Response = string(response)
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

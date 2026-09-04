package mora

// These are the kernel-side witnesses for graph node N21. The listener's own
// properties — the reservation ordering, the byte-identical replay, the conflict,
// the bounds, the revocation race — are proved in internal/companion against the
// package that owns them. What is proved here is the half this package owns:
// that a capture reaches the SAME governed write path `mora write` uses, that
// the vault's write policy is what decides the outcome, that `applied` is a
// statement about a file that exists AND is durable, that the pinned id makes
// the write exactly-once, that the write appears in the usage ledger, and that
// the read-only marker the listener puts on every READ is deliberately absent
// from the one route that writes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/companion"
	loopbackhttp "github.com/pyranthus-hq/mora/internal/loopbackhttp"
)

// captureTestVault is a temp home with an initialized vault and a chosen write
// policy. The policy goes through the same config file an operator edits, so a
// test cannot set a policy the product cannot.
func captureTestVault(t *testing.T, policy string) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = policy
	if err := writeConfig(cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return mustConfig(t)
}

// captureFixture is one valid capture from a paired phone.
func captureFixture(t *testing.T, deviceID, key, text string) companion.Capture {
	t.Helper()
	c := companion.NewCapture()
	c.IdempotencyKey = key
	c.DeviceID = deviceID
	c.CapturedAt = time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	c.RequestedLane = companion.LaneMemory
	c.Intent = companion.IntentRemember
	c.Scope = "personal"
	c.Text = text
	c.PayloadFingerprint = companion.Fingerprint(text)
	if err := c.Validate(); err != nil {
		t.Fatalf("the fixture is not a valid capture: %v", err)
	}
	return c
}

const (
	captureTestDevice = "dev_20260904_030000_a1b2c3d4"
	// captureTestMemoryID stands in for the id the listener derives. Its SHAPE
	// is what matters here — the kernel is handed a pinned id and must publish
	// under exactly it.
	captureTestMemoryID = "mem_20260904_030000_a1b2c3d4"
)

// captureTestIdentity is the CaptureIdentity the listener would hand the kernel
// for a pinned id. The digests only have to be well-formed and stable: what the
// kernel does with them is record ownership and compare it.
func captureTestIdentity(memoryID string) companion.CaptureIdentity {
	return companion.CaptureIdentity{
		DeviceID:    captureTestDevice,
		Key:         "key.one",
		Identity:    companion.Fingerprint("identity:" + memoryID),
		Fingerprint: companion.Fingerprint("payload:" + memoryID),
		MemoryID:    memoryID,
	}
}

// vaultMemories returns every memory in the vault, so a test can count them.
// "Exactly one artefact" is a claim about the vault, not about a receipt.
func vaultMemories(t *testing.T, cfg Config) []Memory {
	t.Helper()
	memories, err := listMemories(cfg, "", 0)
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	return memories
}

// vaultMemoryFiles counts the .md files under the vault. It is deliberately a
// FILE count rather than an API count: "exactly one memory file" is the claim,
// and an API that deduplicated would hide the defect this exists to catch.
func vaultMemoryFiles(t *testing.T, cfg Config) []string {
	t.Helper()
	// Only the memories tree. `mora init` scaffolds control documents beside it
	// — the log, the heartbeat, the priority map — and counting those would make
	// this assertion about the scaffolding rather than about the capture.
	root := filepath.Join(cfg.VaultDir, "memories")
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the memories tree: %v", err)
	}
	return out
}

func proposalCount(t *testing.T, cfg Config) int {
	t.Helper()
	proposals, err := listMCPWriteProposals(cfg)
	if err != nil {
		t.Fatalf("list proposals: %v", err)
	}
	return len(proposals)
}

// ---------------------------------------------------------------------------
// The policy gate, against a real vault
// ---------------------------------------------------------------------------

// TestCompanionCaptureUnderOpenAppliesToTheVault is the `open` row of N02's
// table, proved against the filesystem rather than against a receipt field.
func TestCompanionCaptureUnderOpenAppliesToTheVault(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "the wifi code is on the fridge"), captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if outcome.State != companion.ReceiptApplied {
		t.Fatalf("state = %q, want applied", outcome.State)
	}
	if outcome.Policy != companion.PolicyOpen {
		t.Fatalf("policy = %q, want open", outcome.Policy)
	}
	if outcome.MemoryID == "" {
		t.Fatal("an applied outcome named no memory")
	}

	memories := vaultMemories(t, cfg)
	if len(memories) != 1 {
		t.Fatalf("the vault holds %d memories, want exactly 1", len(memories))
	}
	m := memories[0]
	if m.Text != "the wifi code is on the fridge" {
		t.Fatalf("the vault holds %q, want the captured text", m.Text)
	}
	// The provenance is the kernel's. A device that could set this could make its
	// writes look like the CLI's.
	if m.Source != companion.OriginCompanion {
		t.Fatalf("source = %q, want %q", m.Source, companion.OriginCompanion)
	}
	if m.Scope != "personal" {
		t.Fatalf("scope = %q, want the capture's own scope", m.Scope)
	}
	if _, err := findMemory(cfg, m.ID); err != nil {
		t.Fatalf("the applied memory is not readable: %v", err)
	}
}

// TestCompanionCapturePublishesUnderThePinnedID is the exactly-once primitive
// seen from the kernel side.
//
// The listener derives the id before it reserves the key, so the id the kernel
// is handed IS the vault path. If the kernel minted its own, every retry would
// aim somewhere new and the create-exclusive publish would have nothing to
// refuse.
func TestCompanionCapturePublishesUnderThePinnedID(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	if _, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "pin me"), captureTestIdentity(captureTestMemoryID)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	memories := vaultMemories(t, cfg)
	if len(memories) != 1 {
		t.Fatalf("the vault holds %d memories, want 1", len(memories))
	}
	if memories[0].ID != captureTestMemoryID {
		t.Fatalf("the vault minted %q, want the pinned %q", memories[0].ID, captureTestMemoryID)
	}
}

// TestCompanionCaptureSecondPublishAtTheSameIDWritesOneFile is the defect the
// judge found, closed at the primitive rather than at the bookkeeping.
//
// This is the post-publication crash reduced to its essence: the kernel is asked
// twice to publish the same capture at the same id, exactly as a retry after a
// crash between the write and the receipt would. One file comes out, and the
// second call still answers `applied` — because the memory it was asked for does
// exist.
func TestCompanionCaptureSecondPublishAtTheSameIDWritesOneFile(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()
	capture := captureFixture(t, captureTestDevice, "key.one", "written once")

	first, err := writer.Publish(testCtx(t), capture, captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	second, err := writer.Publish(testCtx(t), capture, captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if first.State != companion.ReceiptApplied || second.State != companion.ReceiptApplied {
		t.Fatalf("states = %q then %q, want applied twice", first.State, second.State)
	}
	if first.MemoryID != second.MemoryID {
		t.Fatalf("the two publishes named %q and %q", first.MemoryID, second.MemoryID)
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 1 {
		t.Fatalf("two publishes at one pinned id left %d memory files, want exactly 1:\n%v", len(files), files)
	}
}

// TestCompanionCapturePublishedReportsTheVault. The takeover pre-check has to
// answer about the VAULT, not about a cache: it is what lets a reclaimed
// reservation settle a receipt for a write it did not make.
func TestCompanionCapturePublishedReportsTheVault(t *testing.T) {
	captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()
	// ONE capture throughout. Published verifies that the memory at the pinned id
	// is THIS capture's, so asking about it with a different body is a different
	// question — and gets a different, correct, answer.
	capture := captureFixture(t, captureTestDevice, "key.one", "now it exists")
	id := captureTestIdentity(captureTestMemoryID)

	if _, published, err := writer.Published(testCtx(t), capture, id); err != nil || published {
		t.Fatalf("an unwritten id reported published=%t err=%v", published, err)
	}
	if _, err := writer.Publish(testCtx(t), capture, id); err != nil {
		t.Fatalf("publish: %v", err)
	}
	outcome, published, err := writer.Published(testCtx(t), capture, id)
	if err != nil || !published {
		t.Fatalf("a written id reported published=%t err=%v", published, err)
	}
	if outcome.State != companion.ReceiptApplied || outcome.Policy != companion.PolicyOpen {
		t.Fatalf("outcome = %+v, want an applied receipt under open", outcome)
	}
	if outcome.MemoryID != companionOpaqueID(companion.PrefixMemory, captureTestMemoryID) {
		t.Fatalf("outcome names %q, want the derived wire identifier", outcome.MemoryID)
	}
}

// TestCompanionCaptureMemoryIDMatchesAnEvidenceRow. A phone that captures a note
// and later sees it in a Today item or a context bundle must be able to tell it
// is the same memory. Both sides derive the wire identifier the same one-way
// way, so the receipt and the evidence row agree without either carrying a
// vault id.
func TestCompanionCaptureMemoryIDMatchesAnEvidenceRow(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "a note to find again"), captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	memories := vaultMemories(t, cfg)
	if len(memories) != 1 {
		t.Fatalf("the vault holds %d memories, want 1", len(memories))
	}
	want := companionOpaqueID(companion.PrefixMemory, memories[0].ID)
	if outcome.MemoryID != want {
		t.Fatalf("the receipt names %q, an evidence row would name %q", outcome.MemoryID, want)
	}
	if strings.Contains(outcome.MemoryID, memories[0].ID) {
		t.Fatalf("the wire identifier carries the vault id: %q", outcome.MemoryID)
	}
}

// TestCompanionCaptureUnderProposeStagesAndWritesNothing is the `propose` row.
func TestCompanionCaptureUnderProposeStagesAndWritesNothing(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyPropose)
	writer := newCompanionWriter()

	before := len(vaultMemories(t, cfg))
	outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "stage me"), captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if outcome.State != companion.ReceiptAccepted {
		t.Fatalf("state = %q, want accepted", outcome.State)
	}
	if outcome.MemoryID != "" {
		t.Fatalf("an accepted outcome named memory %q; nothing is in the vault yet", outcome.MemoryID)
	}
	if got := len(vaultMemories(t, cfg)); got != before {
		t.Fatalf("propose wrote the vault: %d memories, want %d", got, before)
	}
	if got := proposalCount(t, cfg); got != 1 {
		t.Fatalf("propose staged %d proposals, want 1", got)
	}
}

// TestCompanionCaptureUnderReadonlyTouchesNothing is the `readonly` row.
func TestCompanionCaptureUnderReadonlyTouchesNothing(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyReadonly)
	writer := newCompanionWriter()

	before := len(vaultMemories(t, cfg))
	outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "do not write me"), captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if outcome.State != companion.ReceiptRejected || outcome.Reason != companion.ReasonPolicy {
		t.Fatalf("readonly produced %s/%s, want rejected/policy", outcome.State, outcome.Reason)
	}
	if got := len(vaultMemories(t, cfg)); got != before {
		t.Fatalf("readonly wrote the vault: %d memories, want %d", got, before)
	}
	if got := proposalCount(t, cfg); got != 0 {
		t.Fatalf("readonly staged %d proposals, want 0", got)
	}
}

// TestCompanionCaptureUnreadableConfigFailsClosed is the fail-closed gate,
// driven with a config file the product genuinely cannot parse.
//
// It must be a terminal REJECTION rather than an error. An error became a 503
// and left the reservation pending, so an unreadable vault turned every capture
// into a claim nothing could ever settle — the store filled up with them.
func TestCompanionCaptureUnreadableConfigFailsClosed(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	// The same shape TestMCPWritePolicyConfigRoundTripAndRejectsInvalid uses: a
	// policy value the loader refuses, so loadConfigFor errs for real.
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "config.toml"), []byte("mcp_write_policy = \"trust-me\"\n"), 0o600); err != nil {
		t.Fatalf("break the config: %v", err)
	}
	if _, err := loadConfigFor(testCtx(t)); err == nil {
		t.Fatal("the fixture did not actually break the config")
	}

	outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "unreadable config"), captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("an unreadable config produced an error rather than a rejection: %v", err)
	}
	if outcome.State != companion.ReceiptRejected || outcome.Reason != companion.ReasonPolicy {
		t.Fatalf("an unreadable config produced %s/%s, want rejected/policy", outcome.State, outcome.Reason)
	}
	if outcome.Policy != companion.PolicyReadonly {
		t.Fatalf("policy = %q, want the fail-closed readonly", outcome.Policy)
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 0 {
		t.Fatalf("an unreadable config wrote %d memory files", len(files))
	}
}

// TestCompanionCapturePolicyIsReadPerRequest. An operator who runs
// `mora config mcp-write-policy readonly` while the listener is up has made a
// security decision, and a listener answering from a policy it read at boot
// would keep accepting captures until somebody restarted it.
func TestCompanionCapturePolicyIsReadPerRequest(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	first, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "before the flip"), captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if first.State != companion.ReceiptApplied {
		t.Fatalf("state = %q, want applied", first.State)
	}

	cfg.MCPWritePolicy = mcpWritePolicyReadonly
	if err := writeConfig(cfg); err != nil {
		t.Fatalf("flip the policy: %v", err)
	}

	// The SAME writer value, no restart.
	second, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.two", "after the flip"), captureTestIdentity("mem_20260904_030000_bbbbbbbb"))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if second.State != companion.ReceiptRejected || second.Reason != companion.ReasonPolicy {
		t.Fatalf("after the flip: %s/%s, want rejected/policy", second.State, second.Reason)
	}
	if got := len(vaultMemories(t, mustConfig(t))); got != 1 {
		t.Fatalf("the vault holds %d memories, want the 1 written before the flip", got)
	}
}

// ---------------------------------------------------------------------------
// Durability
// ---------------------------------------------------------------------------

// TestCompanionCaptureSyncsThePublicationBeforeItReturns is the durability gate.
//
// A vault write is a rename, and a rename is atomic without being durable: the
// bytes and the directory entry can both still be in cache when the write path
// returns. A receipt that says `applied` is a promise about stable storage, so
// the file and its parent are synced before the outcome goes back to the
// listener — which is strictly before the receipt settles.
func TestCompanionCaptureSyncsThePublicationBeforeItReturns(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	var synced []string
	original := companionSyncPublication
	companionSyncPublication = func(path string) error {
		synced = append(synced, path)
		return original(path)
	}
	t.Cleanup(func() { companionSyncPublication = original })

	if _, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "make me durable"), captureTestIdentity(captureTestMemoryID)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Two things are made durable, in this order: the record that says which
	// capture owns the pinned id, and then the memory itself. The ownership
	// record goes first on purpose — a crash between them leaves an owned id
	// with no memory, which a retry completes, where the other order would leave
	// a memory that looked unowned to its own retry.
	if len(synced) != 2 {
		t.Fatalf("the publication was synced %d times, want 2 (the ownership record, then the memory)", len(synced))
	}
	memories := vaultMemories(t, cfg)
	if len(memories) != 1 {
		t.Fatalf("the vault holds %d memories, want 1", len(memories))
	}
	if synced[0] != capturePublicationPath(cfg, captureTestMemoryID) {
		t.Fatalf("synced %q first, want the ownership record", synced[0])
	}
	if synced[1] != memories[0].Path {
		t.Fatalf("synced %q second, want the memory's own path %q", synced[1], memories[0].Path)
	}
}

// TestCompanionCaptureSyncFailureIsNotAnAppliedReceipt. A publication that
// cannot be made durable is not a publication a receipt may claim: the honest
// answer is to leave the capture unsettled so a retry re-runs the check, which
// the pinned id makes cheap and safe.
func TestCompanionCaptureSyncFailureIsNotAnAppliedReceipt(t *testing.T) {
	captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	original := companionSyncPublication
	companionSyncPublication = func(string) error { return errors.New("the volume went away") }
	t.Cleanup(func() { companionSyncPublication = original })

	outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "undurable"), captureTestIdentity(captureTestMemoryID))
	if err == nil {
		t.Fatalf("a failed sync produced outcome %+v rather than an error", outcome)
	}
	if outcome.State == companion.ReceiptApplied {
		t.Fatal("a failed sync still claimed applied")
	}
}

// TestCompanionCaptureSyncsBeforeTheReservationSettles is the ordering claim,
// driven end to end.
//
// The fake sync reads the reservation file at the instant it runs. It must still
// say `pending`: a store that settled first would be promising durability it had
// not yet obtained.
func TestCompanionCaptureSyncsBeforeTheReservationSettles(t *testing.T) {
	handler, token, cfg, _ := captureListener(t, mcpWritePolicyOpen)
	capture := captureFixture(t, companionListenerDevice(t, cfg), "key.order", "ordering matters")

	var (
		stateAtSync string
		fileAtSync  bool
	)
	original := companionSyncPublication
	companionSyncPublication = func(path string) error {
		// The file must already BE there — syncing before the write would be
		// syncing nothing — and the reservation must not have settled yet.
		_, statErr := os.Stat(path)
		fileAtSync = statErr == nil
		stateAtSync = reservationStateFromDisk(t, cfg, capture.DeviceID, capture.IdempotencyKey)
		return original(path)
	}
	t.Cleanup(func() { companionSyncPublication = original })

	rec := postCompanionCapture(t, handler, token, capture)
	if rec.Code != http.StatusOK {
		t.Fatalf("capture answered %d\n%s", rec.Code, rec.Body.String())
	}
	if !fileAtSync {
		t.Fatal("the sync ran before the memory file existed; it synced nothing")
	}
	if stateAtSync != "pending" {
		t.Fatalf("at the moment of the sync the reservation was %q, want pending — the receipt settled before the bytes were durable", stateAtSync)
	}
	if got := reservationStateFromDisk(t, cfg, capture.DeviceID, capture.IdempotencyKey); got != "settled" {
		t.Fatalf("after the capture the reservation is %q, want settled", got)
	}
}

// reservationStateFromDisk reads the companion reservation's state out of its
// file. The record's Go type is internal to internal/companion, so this reads
// the JSON — which is the right level anyway: what survives a process is bytes.
func reservationStateFromDisk(t *testing.T, cfg Config, deviceID, key string) string {
	t.Helper()
	digest := strings.TrimPrefix(companion.Fingerprint(key), "sha256:")
	path := filepath.Join(cfg.StateDir, "companion", "captures", deviceID, digest+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	var record struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatalf("decode reservation: %v", err)
	}
	return record.State
}

// ---------------------------------------------------------------------------
// The usage ledger
// ---------------------------------------------------------------------------

// TestCompanionCaptureRecordsTheUsageLedgerRow.
//
// The capture write cannot go through invokeMCPTool — the MCP tool table pins
// the handler signature, so the pinned id has nowhere to travel — so it builds
// the same mcpToolInvocation and lets it log. A capture that wrote the vault and
// left no row would make the usage ledger a record of SOME writes rather than of
// writes, which is exactly the kind of quiet gap the ledger exists to close.
func TestCompanionCaptureRecordsTheUsageLedgerRow(t *testing.T) {
	for _, tc := range []struct {
		policy string
		state  companion.ReceiptState
	}{
		{mcpWritePolicyOpen, companion.ReceiptApplied},
		{mcpWritePolicyPropose, companion.ReceiptAccepted},
		{mcpWritePolicyReadonly, companion.ReceiptRejected},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			cfg := captureTestVault(t, tc.policy)
			writer := newCompanionWriter()

			outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "ledger me"), captureTestIdentity(captureTestMemoryID))
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
			if outcome.State != tc.state {
				t.Fatalf("state = %q, want %q", outcome.State, tc.state)
			}
			raw := readUsageLog(t, cfg)
			if !strings.Contains(raw, `"tool":"write_memory"`) {
				t.Fatalf("the capture left no write_memory row in the usage ledger:\n%s", raw)
			}
			// The ledger is content-free, and a capture must not be the thing that
			// changes that.
			if strings.Contains(raw, "ledger me") {
				t.Fatalf("the capture's text reached the usage ledger:\n%s", raw)
			}
		})
	}
}

// TestCompanionCaptureLedgerRowMatchesTheMCPToolRow. "The same ledger" has to
// mean the same SHAPE, or a reader would have to know which writes came from a
// phone to parse the file.
func TestCompanionCaptureLedgerRowMatchesTheMCPToolRow(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)

	if _, err := callMCPTool(testCtx(t), "write_memory", map[string]any{
		"title": "From MCP", "text": "an agent wrote this",
	}); err != nil {
		t.Fatalf("write_memory: %v", err)
	}
	writer := newCompanionWriter()
	if _, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "a phone wrote this"), captureTestIdentity(captureTestMemoryID)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	rows := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(readUsageLog(t, cfg)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("decode usage row %q: %v", line, err)
		}
		if row["tool"] == "write_memory" {
			rows = append(rows, row)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("the ledger holds %d write_memory rows, want 2 (one MCP, one capture)", len(rows))
	}
	agent, phone := rows[0], rows[1]
	for key := range agent {
		if _, ok := phone[key]; !ok {
			t.Fatalf("the capture's ledger row is missing %q, which the MCP row carries", key)
		}
	}
	for key := range phone {
		if _, ok := agent[key]; !ok {
			t.Fatalf("the capture's ledger row carries %q, which the MCP row does not", key)
		}
	}
}

// ---------------------------------------------------------------------------
// The read-only marker
// ---------------------------------------------------------------------------

// readOnlyProbe wraps the real writer and records whether the context the
// listener handed it forbids durable work.
type readOnlyProbe struct {
	mu       sync.Mutex
	inner    companion.Writer
	readOnly bool
	seen     bool
}

func (p *readOnlyProbe) Policy(ctx context.Context) (companion.WritePolicy, error) {
	return p.inner.Policy(ctx)
}

func (p *readOnlyProbe) PublishedForKey(ctx context.Context, deviceID, key string) (string, bool, error) {
	return p.inner.PublishedForKey(ctx, deviceID, key)
}

func (p *readOnlyProbe) Published(ctx context.Context, c companion.Capture, id companion.CaptureIdentity) (companion.WriteOutcome, bool, error) {
	return p.inner.Published(ctx, c, id)
}

func (p *readOnlyProbe) Publish(ctx context.Context, c companion.Capture, id companion.CaptureIdentity) (companion.WriteOutcome, error) {
	p.mu.Lock()
	p.readOnly, p.seen = readOnlyCall(ctx), true
	p.mu.Unlock()
	return p.inner.Publish(ctx, c, id)
}

// TestCompanionCaptureIsNotMarkedReadOnly is the N12 invariant, stated for the
// exception.
func TestCompanionCaptureIsNotMarkedReadOnly(t *testing.T) {
	handler, token, cfg, probe := captureListener(t, mcpWritePolicyOpen)

	rec := postCompanionCapture(t, handler, token, captureFixture(t, companionListenerDevice(t, cfg), "key.one", "write me"))
	if rec.Code != http.StatusOK {
		t.Fatalf("capture answered %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	probe.mu.Lock()
	seen, readOnly := probe.seen, probe.readOnly
	probe.mu.Unlock()
	if !seen {
		t.Fatal("the capture never reached the writer")
	}
	if readOnly {
		t.Fatal("the capture ran under the read-only kernel marker; the one write route would refuse itself")
	}
}

// TestCompanionWriterNeverMarksItsContextReadOnly is the same invariant read out
// of the source.
//
// The behavioural test above measures what the LISTENER hands the writer. This
// one measures the other end: that the writer does not set the marker on itself.
// It is a source check because the marker is currently latent on the write path
// — no repair site along createMemory consults it — so a capture marked
// read-only would still succeed today and start failing silently the moment one
// did. A witness that only fires after the breakage has shipped is not a
// witness.
func TestCompanionWriterNeverMarksItsContextReadOnly(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "companion_http.go", nil, 0)
	if err != nil {
		t.Fatalf("parse companion_http.go: %v", err)
	}
	forbidden := map[string]bool{"companionKernelContext": true, "withReadOnly": true}
	checked := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "companionWriter" {
			continue
		}
		checked++
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := call.Fun.(*ast.Ident); ok && forbidden[name.Name] {
				t.Fatalf("companionWriter.%s calls %s; the one write route would mark itself read-only", fn.Name.Name, name.Name)
			}
			return true
		})
	}
	// The three the Writer seam declares, plus the two the verification is made
	// of. The count is asserted so a method added later is walked rather than
	// silently skipped by this witness.
	if checked != 6 {
		t.Fatalf("walked %d companionWriter methods, want 6", checked)
	}
}

// TestCompanionReadRoutesStayMarkedReadOnly is the other half: widening the
// listener to a write route must not have widened the reads.
func TestCompanionReadRoutesStayMarkedReadOnly(t *testing.T) {
	captureTestVault(t, mcpWritePolicyOpen)
	if !readOnlyCall(companionKernelContext(testCtx(t))) {
		t.Fatal("the companion read context is no longer marked read-only")
	}
	if readOnlyCall(testCtx(t)) {
		t.Fatal("an ordinary kernel context is marked read-only")
	}
}

// ---------------------------------------------------------------------------
// End to end, through the real listener
// ---------------------------------------------------------------------------

// captureListener assembles the production listener over a real vault, with the
// real registry, the real writer behind a probe, and the real reservation store.
func captureListener(t *testing.T, policy string) (http.Handler, string, Config, *readOnlyProbe) {
	t.Helper()
	cfg := captureTestVault(t, policy)
	reg := companion.NewRegistry(cfg.ConfigDir, cfg.StateDir,
		companion.WithClock(func() time.Time { return cfg.OperationClock() }))
	payload, err := reg.Pair("phone", companion.PlatformIOS,
		fmt.Sprintf("http://%s:%d", companion.LoopbackHost, defaultCompanionPort))
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	confirmation := companion.NewPairingConfirmation()
	confirmation.DeviceID = payload.DeviceID
	confirmation.PairingCode = payload.PairingCode
	confirmation.Label = "phone"
	confirmation.Platform = companion.PlatformIOS
	confirmation.PublicKey = "ed25519:" + strings.Repeat("A", 43) + "="
	confirmation.ConfirmedAt = payload.ExpiresAt
	token, _, err := reg.Confirm(confirmation)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	probe := &readOnlyProbe{inner: newCompanionWriter()}
	srv, err := companion.NewServer(companion.ServerOptions{
		Addr:     fmt.Sprintf("%s:%d", companion.LoopbackHost, defaultCompanionPort),
		Devices:  reg,
		Reader:   newCompanionReader(cfg),
		Writer:   probe,
		Captures: companion.NewReservationStore(cfg.StateDir, companion.WithReservationClock(cfg.OperationClock)),
		Now:      cfg.OperationClock,
	})
	if err != nil {
		t.Fatalf("new companion server: %v", err)
	}
	return srv.Handler(), token, cfg, probe
}

// companionListenerDevice returns the one device captureListener paired.
func companionListenerDevice(t *testing.T, cfg Config) string {
	t.Helper()
	reg := companion.NewRegistry(cfg.ConfigDir, cfg.StateDir)
	devices, err := reg.List()
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("the registry holds %d devices, want 1", len(devices))
	}
	return devices[0].DeviceID
}

func postCompanionCapture(t *testing.T, handler http.Handler, token string, c companion.Capture) *httptest.ResponseRecorder {
	t.Helper()
	body, err := companion.Marshal(&c)
	if err != nil {
		t.Fatalf("marshal capture: %v", err)
	}
	rec := httptest.NewRecorder()
	// The request carries the test environment on its CONTEXT, which is exactly
	// how production works: `mora companion serve` sets the http.Server's
	// BaseContext to the command's own context, so every request the writer sees
	// descends from it. A request built with a bare background context would be
	// testing a wiring the product does not have.
	handler.ServeHTTP(rec, companionRequest(t, http.MethodPost, companion.RouteCapture, token, string(body)).WithContext(testCtx(t)))
	return rec
}

// TestCompanionCaptureEndToEndRetriesWriteOnce is the whole node in one test: a
// real phone request, a real vault, a real reservation store, and a retry.
func TestCompanionCaptureEndToEndRetriesWriteOnce(t *testing.T) {
	handler, token, cfg, _ := captureListener(t, mcpWritePolicyOpen)
	capture := captureFixture(t, companionListenerDevice(t, cfg), "key.retry", "quokkas assemble seventeen hundred")

	first := postCompanionCapture(t, handler, token, capture)
	if first.Code != http.StatusOK {
		t.Fatalf("first capture answered %d\n%s", first.Code, first.Body.String())
	}
	second := postCompanionCapture(t, handler, token, capture)
	if second.Code != http.StatusOK {
		t.Fatalf("the retry answered %d\n%s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("the retry returned different bytes\n%s\n%s", first.Body.String(), second.Body.String())
	}

	var receipt companion.Receipt
	if err := json.Unmarshal(first.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("the listener returned an invalid receipt: %v", err)
	}
	if receipt.State != companion.ReceiptApplied {
		t.Fatalf("state = %q, want applied", receipt.State)
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 1 {
		t.Fatalf("a capture and its retry produced %d memory files, want exactly 1:\n%v", len(files), files)
	}
	// Short words are skipped: a two-letter fragment appears inside identifiers
	// by coincidence, and a witness that fails on coincidence stops meaning
	// anything. Every word in this capture is long enough to be a real leak.
	for _, word := range strings.Fields("quokkas assemble seventeen hundred") {
		if strings.Contains(first.Body.String(), word) {
			t.Fatalf("the receipt echoed %q", word)
		}
	}
}

// TestCompanionCaptureEndToEndCrashAfterPublicationLeavesOneMemoryFile is the
// judge's blocking defect, end to end against a real vault.
//
// The vault write lands; the process dies before the receipt settles. A restart
// retries the same request past the takeover window. The vault must hold ONE
// memory file, the retry must answer `applied`, and a third attempt must replay
// that receipt rather than mint another.
func TestCompanionCaptureEndToEndCrashAfterPublicationLeavesOneMemoryFile(t *testing.T) {
	handler, token, cfg, _ := captureListener(t, mcpWritePolicyOpen)
	device := companionListenerDevice(t, cfg)
	capture := captureFixture(t, device, "key.crash", "survives the kill")

	// The kill: the memory is published and made durable, and then the process
	// never gets to write the receipt. Failing the sync is the last thing before
	// the settle that leaves the vault written and the reservation pending.
	original := companionSyncPublication
	companionSyncPublication = func(path string) error {
		if err := original(path); err != nil {
			return err
		}
		if strings.HasSuffix(path, ".json") {
			// The ownership record's own sync. The kill has to land AFTER the
			// memory exists, so this one is allowed through.
			return nil
		}
		return errors.New("the process died before the receipt settled")
	}
	if rec := postCompanionCapture(t, handler, token, capture); rec.Code != http.StatusServiceUnavailable {
		companionSyncPublication = original
		t.Fatalf("the crashing attempt answered %d, want 503\n%s", rec.Code, rec.Body.String())
	}
	companionSyncPublication = original

	if files := vaultMemoryFiles(t, cfg); len(files) != 1 {
		t.Fatalf("the crashed attempt left %d memory files, want 1", len(files))
	}
	if state := reservationStateFromDisk(t, cfg, device, capture.IdempotencyKey); state != "pending" {
		t.Fatalf("after the crash the reservation is %q, want pending", state)
	}

	// The restart: a second listener over the same vault, the same registry and
	// the same reservation directory, with its clock past the takeover window.
	after := cfg.OperationClock().Add(companion.ReservationTakeover + time.Second)
	restarted := restartedCaptureListener(t, cfg, after)

	rec := postCompanionCapture(t, restarted, token, capture)
	if rec.Code != http.StatusOK {
		t.Fatalf("the retry answered %d\n%s", rec.Code, rec.Body.String())
	}
	var receipt companion.Receipt
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if receipt.State != companion.ReceiptApplied {
		t.Fatalf("the retry produced %q, want applied", receipt.State)
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 1 {
		t.Fatalf("the retry left %d memory files, want exactly 1:\n%v", len(files), files)
	}

	third := postCompanionCapture(t, restarted, token, capture)
	if third.Body.String() != rec.Body.String() {
		t.Fatalf("a third attempt answered differently\n%s\n%s", rec.Body.String(), third.Body.String())
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 1 {
		t.Fatalf("a third attempt left %d memory files, want 1", len(files))
	}
}

// restartedCaptureListener builds a second listener over an existing vault's
// registry and reservation directory, with a clock of its own. That is what a
// restarted `mora companion serve` is.
func restartedCaptureListener(t *testing.T, cfg Config, now time.Time) http.Handler {
	t.Helper()
	clock := func() time.Time { return now }
	reg := companion.NewRegistry(cfg.ConfigDir, cfg.StateDir, companion.WithClock(clock))
	srv, err := companion.NewServer(companion.ServerOptions{
		Addr:     fmt.Sprintf("%s:%d", companion.LoopbackHost, defaultCompanionPort),
		Devices:  reg,
		Reader:   newCompanionReader(cfg),
		Writer:   newCompanionWriter(),
		Captures: companion.NewReservationStore(cfg.StateDir, companion.WithReservationClock(clock)),
		Now:      clock,
	})
	if err != nil {
		t.Fatalf("restart the listener: %v", err)
	}
	return srv.Handler()
}

// TestCompanionCaptureRefusesTheGenericLoopbackToken. The two credential
// families are disjoint, and a write route is where that matters most.
func TestCompanionCaptureRefusesTheGenericLoopbackToken(t *testing.T) {
	handler, _, cfg, _ := captureListener(t, mcpWritePolicyOpen)
	capture := captureFixture(t, companionListenerDevice(t, cfg), "key.one", "not yours to write")

	body, err := companion.Marshal(&capture)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	loopbackToken, err := loopbackhttp.LoadOrCreateToken(cfg.ConfigDir)
	if err != nil {
		t.Fatalf("load loopback token: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, companionRequest(t, http.MethodPost, companion.RouteCapture, loopbackToken, string(body)).WithContext(testCtx(t)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the generic loopback token wrote through the companion listener: %d\n%s", rec.Code, rec.Body.String())
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 0 {
		t.Fatalf("an unauthenticated capture wrote %d memory files", len(files))
	}
}

// ---------------------------------------------------------------------------
// A pinned id the vault holds against somebody else
// ---------------------------------------------------------------------------

// TestCompanionCaptureForeignFileAtThePinnedIDIsRejected is the verified-EEXIST
// gate, against a real vault.
//
// Round two took the create-exclusive publish's EEXIST as proof that the capture
// was already published. EEXIST says a file is there; it says nothing about
// whose. A tampered vault, or a collision in the 32-bit suffix, therefore
// produced a confident `applied` receipt for a memory the phone never wrote.
//
// The file is pre-created with different content, so the ownership record and
// the content comparison both disagree with the capture claiming the id.
func TestCompanionCaptureForeignFileAtThePinnedIDIsRejected(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	// Somebody else's memory, already at the pinned path.
	foreign := Memory{
		Scope: "personal", Type: "insight", Title: "Not the capture",
		Source: "cli", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Text: "this file was here first", ID: captureTestMemoryID,
	}
	body, err := renderMemory(foreign)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	path := memoryPath(cfg, foreign)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed the vault: %v", err)
	}

	outcome, err := writer.Publish(testCtx(t),
		captureFixture(t, captureTestDevice, "key.one", "my note"),
		captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if outcome.State != companion.ReceiptRejected || outcome.Reason != companion.ReasonInternal {
		t.Fatalf("a foreign file at the pinned id produced %s/%s, want rejected/internal", outcome.State, outcome.Reason)
	}
	if outcome.MemoryID != "" {
		t.Fatalf("a rejected outcome named memory %q", outcome.MemoryID)
	}
	// The foreign file is untouched: refusing means refusing, not overwriting.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != string(body) {
		t.Fatal("the refusal overwrote the file it refused")
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 1 {
		t.Fatalf("the vault holds %d memory files, want the 1 that was already there", len(files))
	}
	// An operator can find out WHY. The reason on the wire is the frozen
	// `internal`; the specific cause travels on the outcome, naming the ids and
	// nothing the user wrote, and the LISTENER decides where it goes — the kernel
	// has no log of its own on this path, and giving it the listener's stdout is
	// what regressed N12's silence.
	if !strings.Contains(outcome.IntegrityDetail, captureTestMemoryID) ||
		!strings.Contains(outcome.IntegrityDetail, captureTestDevice) {
		t.Fatalf("the integrity detail names neither the memory nor the device: %q", outcome.IntegrityDetail)
	}
	if strings.Contains(outcome.IntegrityDetail, "my note") || strings.Contains(outcome.IntegrityDetail, "this file was here first") {
		t.Fatalf("the integrity detail carries memory text: %q", outcome.IntegrityDetail)
	}
}

// TestCompanionCaptureOwnFileAtThePinnedIDIsAppliedWithoutASecondWrite is the
// other half. Verification must not turn our OWN publication into a rejection:
// a retry that finds its own memory already there is the exactly-once success
// case, and it must not write again.
func TestCompanionCaptureOwnFileAtThePinnedIDIsAppliedWithoutASecondWrite(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()
	capture := captureFixture(t, captureTestDevice, "key.one", "mine, written once")
	id := captureTestIdentity(captureTestMemoryID)

	first, err := writer.Publish(testCtx(t), capture, id)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	before, err := os.ReadFile(memoryPath(cfg, Memory{Scope: "personal", Type: "insight", ID: captureTestMemoryID}))
	if err != nil {
		t.Fatalf("read the published memory: %v", err)
	}

	second, err := writer.Publish(testCtx(t), capture, id)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if first.State != companion.ReceiptApplied || second.State != companion.ReceiptApplied {
		t.Fatalf("states = %q then %q, want applied twice", first.State, second.State)
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 1 {
		t.Fatalf("two publishes left %d memory files, want exactly 1", len(files))
	}
	after, err := os.ReadFile(vaultMemoryFiles(t, cfg)[0])
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("the second publish rewrote the memory it found")
	}
}

// TestCompanionCapturePublicationRecordNamesItsOwner. The ownership record is
// what makes "already published" answerable after the reservation that knew it
// has been swept. It is written BEFORE the memory, so our own retry never sees
// an unowned file where its memory should be.
func TestCompanionCapturePublicationRecordNamesItsOwner(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()
	id := captureTestIdentity(captureTestMemoryID)

	if _, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "owned"), id); err != nil {
		t.Fatalf("publish: %v", err)
	}
	record, found, err := readCapturePublication(cfg, captureTestMemoryID)
	if err != nil || !found {
		t.Fatalf("the publication record is missing: found=%t err=%v", found, err)
	}
	if !record.matches(id) {
		t.Fatalf("the record does not describe the capture that wrote it: %+v", record)
	}
	// It carries digests and identifiers, never a word the user wrote.
	body, err := os.ReadFile(capturePublicationPath(cfg, captureTestMemoryID))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), "owned") {
		t.Fatalf("the publication record carries the capture text: %s", body)
	}

	// A DIFFERENT capture cannot claim the same id.
	other := captureTestIdentity(captureTestMemoryID)
	other.Key = "key.two"
	if _, err := claimCapturePublication(cfg, other); !errors.Is(err, errMemoryIDMismatch) {
		t.Fatalf("a second capture claimed the same id: %v", err)
	}
	// And the owner can re-claim its own id as many times as it retries.
	if created, err := claimCapturePublication(cfg, id); err != nil || created {
		t.Fatalf("the owner could not re-claim its own id: created=%t err=%v", created, err)
	}
}

// TestCompanionCapturePublishedVerifiesRatherThanTrusting. The takeover
// pre-check answers the same question as the EEXIST branch and must answer it
// the same way, or a reclaimed reservation would settle a receipt for somebody
// else's memory.
func TestCompanionCapturePublishedVerifiesRatherThanTrusting(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()
	capture := captureFixture(t, captureTestDevice, "key.one", "mine")
	id := captureTestIdentity(captureTestMemoryID)

	if _, err := writer.Publish(testCtx(t), capture, id); err != nil {
		t.Fatalf("publish: %v", err)
	}
	outcome, published, err := writer.Published(testCtx(t), capture, id)
	if err != nil || !published || outcome.State != companion.ReceiptApplied {
		t.Fatalf("the owner's own memory reported published=%t state=%q err=%v", published, outcome.State, err)
	}

	// The same id, asked about by a DIFFERENT capture: present, and not theirs.
	stranger := captureTestIdentity(captureTestMemoryID)
	stranger.Key = "key.two"
	stranger.Identity = companion.Fingerprint("a different capture")
	outcome, published, err = writer.Published(testCtx(t), capture, stranger)
	if err != nil {
		t.Fatalf("published: %v", err)
	}
	if !published {
		t.Fatal("a taken id reported absent to a stranger; the retry would write into it")
	}
	if outcome.State != companion.ReceiptRejected || outcome.Reason != companion.ReasonInternal {
		t.Fatalf("a stranger got %s/%s, want rejected/internal", outcome.State, outcome.Reason)
	}
	_ = cfg
}

// ---------------------------------------------------------------------------
// Nothing written means nothing written
// ---------------------------------------------------------------------------

// publishedTree is the state of the ownership directory, as bytes.
//
// The claim under test is "nothing was written", and that has to be true of the
// state directory as well as of the vault. Comparing the tree byte for byte is
// the only assertion that catches a rejection which tidied up almost everything.
func publishedTree(t *testing.T, cfg Config) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := capturePublicationDir(cfg)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out[rel] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the published tree: %v", err)
	}
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestCompanionCaptureForeignRejectionLeavesNoResidue is the round-three
// residue defect.
//
// The ownership record is written before the vault write, so a capture that then
// finds the memory foreign has already created it. Round three rejected and left
// it there, which made "nothing was written" false of the state directory — and
// worse, let a caller who can grind the 32-bit suffix pre-plant ownership for ids
// nobody has published.
func TestCompanionCaptureForeignRejectionLeavesNoResidue(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	// Somebody else's memory, already at the pinned path, with no ownership
	// record — so the claim below is the one that creates one.
	foreign := Memory{
		Scope: "personal", Type: "insight", Title: "Not the capture",
		Source: "cli", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Text: "this file was here first", ID: captureTestMemoryID,
	}
	body, err := renderMemory(foreign)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	path := memoryPath(cfg, foreign)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed the vault: %v", err)
	}

	before := publishedTree(t, cfg)
	outcome, err := writer.Publish(testCtx(t),
		captureFixture(t, captureTestDevice, "key.one", "my note"),
		captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if outcome.State != companion.ReceiptRejected || outcome.Reason != companion.ReasonInternal {
		t.Fatalf("produced %s/%s, want rejected/internal", outcome.State, outcome.Reason)
	}
	after := publishedTree(t, cfg)
	if !sameTree(before, after) {
		t.Fatalf("the rejection left residue in the published tree:\nbefore %v\nafter  %v", before, after)
	}
	if len(after) != 0 {
		t.Fatalf("the rejected request created %d ownership records, want 0", len(after))
	}
	// And the key was not marked as having published anything.
	if _, found, ferr := publishedForKey(cfg, captureTestDevice, "key.one"); ferr != nil || found {
		t.Fatalf("the rejected request claimed the key: found=%t err=%v", found, ferr)
	}
}

// TestCompanionCaptureForeignOwnerRecordIsNotTouched. When the id is owned by a
// DIFFERENT capture, the refusal must not create anything and must not remove
// the record it does not own.
func TestCompanionCaptureForeignOwnerRecordIsNotTouched(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	// Somebody else already owns the id.
	stranger := captureTestIdentity(captureTestMemoryID)
	stranger.Key = "their.key"
	stranger.Identity = companion.Fingerprint("their capture")
	if _, err := claimCapturePublication(cfg, stranger); err != nil {
		t.Fatalf("seed the ownership: %v", err)
	}
	before := publishedTree(t, cfg)

	outcome, err := writer.Publish(testCtx(t),
		captureFixture(t, captureTestDevice, "key.one", "mine"),
		captureTestIdentity(captureTestMemoryID))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if outcome.State != companion.ReceiptRejected || outcome.Reason != companion.ReasonInternal {
		t.Fatalf("produced %s/%s, want rejected/internal", outcome.State, outcome.Reason)
	}
	if after := publishedTree(t, cfg); !sameTree(before, after) {
		t.Fatalf("the refusal touched a record it does not own:\nbefore %v\nafter  %v", before, after)
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 0 {
		t.Fatalf("the refusal wrote %d memory files", len(files))
	}
}

// TestCompanionCaptureOwnershipIsCreatedExclusively. Reading a record and then
// writing one is a check-then-use race: two processes claiming one id would both
// read "absent" and both write. The create is O_EXCL so the filesystem decides,
// and this asserts the create itself rather than the branch above it.
func TestCompanionCaptureOwnershipIsCreatedExclusively(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	path := capturePublicationPath(cfg, captureTestMemoryID)

	if err := writeCapturePublicationExclusive(path, []byte("first\n")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := writeCapturePublicationExclusive(path, []byte("second\n"))
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("the second create returned %v, want os.ErrExist", err)
	}
	body, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read back: %v", rerr)
	}
	if string(body) != "first\n" {
		t.Fatalf("the refused create overwrote the record: %q", body)
	}
}

// TestCompanionCapturePrePlantedOwnerCannotClaimALaterMemory. An ownership record
// alone proves nothing about the vault: the memory comparison still runs, so a
// record planted for an id nobody published cannot be used to adopt whatever
// turns up at that path later.
func TestCompanionCapturePrePlantedOwnerCannotClaimALaterMemory(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()
	id := captureTestIdentity(captureTestMemoryID)

	// The record is planted first, by the same capture that will later ask.
	if _, err := claimCapturePublication(cfg, id); err != nil {
		t.Fatalf("plant the ownership: %v", err)
	}
	// And somebody else's memory turns up at that id.
	foreign := Memory{
		Scope: "personal", Type: "insight", Title: "Planted",
		Source: "cli", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Text: "not what the capture says", ID: captureTestMemoryID,
	}
	body, err := renderMemory(foreign)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	path := memoryPath(cfg, foreign)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed the vault: %v", err)
	}

	outcome, published, err := writer.Published(testCtx(t),
		captureFixture(t, captureTestDevice, "key.one", "mine"), id)
	if err != nil {
		t.Fatalf("published: %v", err)
	}
	if !published || outcome.State != companion.ReceiptRejected || outcome.Reason != companion.ReasonInternal {
		t.Fatalf("a planted record adopted a foreign memory: published=%t %s/%s", published, outcome.State, outcome.Reason)
	}
}

// TestCompanionCaptureOwnerFsyncedButMemoryAbsentIsCompleted is the crash the
// ordering exists for: the ownership record is durable and the memory is not,
// which is what a kill between the two leaves. The retry must WRITE, not refuse.
func TestCompanionCaptureOwnerFsyncedButMemoryAbsentIsCompleted(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()
	capture := captureFixture(t, captureTestDevice, "key.one", "owner first, memory later")
	id := captureTestIdentity(captureTestMemoryID)

	// The crash: the ownership record lands, the memory never does.
	if _, err := claimCapturePublication(cfg, id); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 0 {
		t.Fatalf("the claim wrote %d memory files, want 0", len(files))
	}

	// The takeover pre-check must say ABSENT: an ownership record is a claim on a
	// path, not a memory, and treating it as published would settle a receipt for
	// a memory that does not exist.
	if _, published, perr := writer.Published(testCtx(t), capture, id); perr != nil || published {
		t.Fatalf("an owner record with no memory reported published=%t err=%v", published, perr)
	}

	outcome, err := writer.Publish(testCtx(t), capture, id)
	if err != nil {
		t.Fatalf("the retry could not complete the crashed publication: %v", err)
	}
	if outcome.State != companion.ReceiptApplied {
		t.Fatalf("the retry produced %q, want applied", outcome.State)
	}
	if files := vaultMemoryFiles(t, cfg); len(files) != 1 {
		t.Fatalf("the retry left %d memory files, want exactly 1", len(files))
	}
}

// TestCompanionCapturePublishedByKeySurvivesAndIsBounded covers the durable
// audit trail: it answers by (device, key), it is trimmed oldest-first at the
// same total cap the reservation store uses, and the trim walks only when the
// directory is over that cap.
func TestCompanionCapturePublishedByKeyIsBounded(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)

	// One over the cap, each a second apart so "oldest" is a real ordering.
	base := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)
	original := mcpWriteClock
	t.Cleanup(func() { mcpWriteClock = original })
	for i := 0; i <= companion.MaxReservations; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		mcpWriteClock = func() time.Time { return at }
		id := companion.CaptureIdentity{
			DeviceID:    captureTestDevice,
			Key:         fmt.Sprintf("key.%04d", i),
			Identity:    companion.Fingerprint(fmt.Sprintf("identity %d", i)),
			Fingerprint: companion.Fingerprint(fmt.Sprintf("payload %d", i)),
			MemoryID:    fmt.Sprintf("mem_%s_%08x", at.Format("20060102_150405"), i),
		}
		if _, err := claimCapturePublication(cfg, id); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(capturePublicationDir(cfg))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	records := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			records++
		}
	}
	if records > companion.MaxReservations {
		t.Fatalf("the published tree holds %d records, over the %d cap", records, companion.MaxReservations)
	}
	// The oldest went; the newest stayed, and it is still answerable by key.
	if _, found, err := publishedForKey(cfg, captureTestDevice, "key.0000"); err != nil || found {
		t.Fatalf("the oldest record survived the trim: found=%t err=%v", found, err)
	}
	newest := fmt.Sprintf("key.%04d", companion.MaxReservations)
	if _, found, err := publishedForKey(cfg, captureTestDevice, newest); err != nil || !found {
		t.Fatalf("the newest record was trimmed: found=%t err=%v", found, err)
	}
}

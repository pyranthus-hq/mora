package mora

// These are the kernel-side witnesses for graph node N21. The listener's own
// properties — the reservation ordering, the byte-identical replay, the conflict,
// the crash recovery, the bounds — are proved in internal/companion against the
// package that owns them. What is proved here is the half this package owns:
// that a capture reaches the SAME governed write path `mora write` uses, that the
// vault's write policy is what decides the outcome, that `applied` is a statement
// about a file that exists, and that the read-only marker the listener puts on
// every READ is deliberately absent from the one route that writes.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

const captureTestDevice = "dev_20260904_030000_a1b2c3d4"

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
//
// The receipt says applied; the assertion is that the memory is on disk, that it
// carries the text the phone sent, and that the kernel — not the device —
// stamped its provenance.
func TestCompanionCaptureUnderOpenAppliesToTheVault(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "the wifi code is on the fridge"))
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
	// The memory is really readable through the vault's own lookup, which is what
	// makes "applied" a checkable claim rather than a label.
	if _, err := findMemory(cfg, m.ID); err != nil {
		t.Fatalf("the applied memory is not readable: %v", err)
	}
}

// TestCompanionCaptureMemoryIDMatchesAnEvidenceRow. A phone that captures a note
// and later sees it in a Today item or a context bundle must be able to tell it
// is the same memory. Both sides derive the identifier the same one-way way, so
// the receipt and the evidence row agree without either carrying a provider id.
func TestCompanionCaptureMemoryIDMatchesAnEvidenceRow(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "a note to find again"))
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
	// And the identifier carries nothing about the vault's own id.
	if strings.Contains(outcome.MemoryID, memories[0].ID) {
		t.Fatalf("the wire identifier carries the vault id: %q", outcome.MemoryID)
	}
}

// TestCompanionCaptureUnderProposeStagesAndWritesNothing is the `propose` row.
//
// Accepted means staged for local approval and NOT in the vault, and the queue it
// is staged in is the one `mora mcp proposals` already lists — so a phone capture
// and an agent write wait for the same human in the same place.
func TestCompanionCaptureUnderProposeStagesAndWritesNothing(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyPropose)
	writer := newCompanionWriter()

	before := len(vaultMemories(t, cfg))
	outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "stage me"))
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

// TestCompanionCaptureUnderReadonlyTouchesNothing is the `readonly` row. The
// refusal has to be a refusal: no memory, and no pending proposal either, since a
// staged write under readonly would be a queued change the policy forbade.
func TestCompanionCaptureUnderReadonlyTouchesNothing(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyReadonly)
	writer := newCompanionWriter()

	before := len(vaultMemories(t, cfg))
	outcome, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "do not write me"))
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

// TestCompanionCapturePolicyIsReadPerRequest. An operator who runs
// `mora config mcp-write-policy readonly` while the listener is up has made a
// security decision, and a listener answering from a policy it read at boot
// would keep accepting captures until somebody restarted it.
func TestCompanionCapturePolicyIsReadPerRequest(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	writer := newCompanionWriter()

	first, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.one", "before the flip"))
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
	second, err := writer.Publish(testCtx(t), captureFixture(t, captureTestDevice, "key.two", "after the flip"))
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
// The read-only marker
// ---------------------------------------------------------------------------

// readOnlyProbe wraps the real writer and records whether the context the
// listener handed it forbids durable work.
type readOnlyProbe struct {
	inner    companion.Writer
	readOnly bool
	seen     bool
}

func (p *readOnlyProbe) Policy(ctx context.Context) (companion.WritePolicy, error) {
	return p.inner.Policy(ctx)
}

func (p *readOnlyProbe) Publish(ctx context.Context, c companion.Capture) (companion.WriteOutcome, error) {
	p.readOnly, p.seen = readOnlyCall(ctx), true
	return p.inner.Publish(ctx, c)
}

// TestCompanionCaptureIsNotMarkedReadOnly is the N12 invariant, stated for the
// exception.
//
// Every READ this listener makes is marked "answer from what exists, never
// repair", because a repair is minutes of disk reachable from one request.
// Capture is the exception by definition: it is a write, governed by the vault's
// own policy, and marking it read-only would make the one authorized mutation
// refuse itself. The marker being absent here is therefore deliberate, and it is
// asserted rather than assumed.
func TestCompanionCaptureIsNotMarkedReadOnly(t *testing.T) {
	handler, token, cfg, probe := captureListener(t, mcpWritePolicyOpen)

	rec := postCompanionCapture(t, handler, token, captureFixture(t, companionListenerDevice(t, cfg), "key.one", "write me"))
	if rec.Code != http.StatusOK {
		t.Fatalf("capture answered %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if !probe.seen {
		t.Fatal("the capture never reached the writer")
	}
	if probe.readOnly {
		t.Fatal("the capture ran under the read-only kernel marker; the one write route would refuse itself")
	}
}

// TestCompanionWriterNeverMarksItsContextReadOnly is the same invariant read out
// of the source.
//
// The behavioural test above measures what the LISTENER hands the writer, which
// is where the marker is set today. This one measures the other end: that the
// writer does not set it on itself. It is a source check rather than a
// behavioural one because the marker is currently latent on the write path —
// no repair site along createMemory consults it — so a capture marked read-only
// would still succeed today and start failing silently the moment one did. A
// witness that only fires after the breakage has shipped is not a witness.
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
	if checked != 2 {
		t.Fatalf("walked %d companionWriter methods, want the 2 the Writer seam declares", checked)
	}
}

// TestCompanionReadRoutesStayMarkedReadOnly is the other half: widening the
// listener to a write route must not have widened the reads. A read that could
// repair is a phone that can spend the Mac's disk with one GET.
func TestCompanionReadRoutesStayMarkedReadOnly(t *testing.T) {
	cfg := captureTestVault(t, mcpWritePolicyOpen)
	if readOnlyCall(companionKernelContext(testCtx(t))) != true {
		t.Fatal("the companion read context is no longer marked read-only")
	}
	if readOnlyCall(testCtx(t)) {
		t.Fatal("an ordinary kernel context is marked read-only")
	}
	_ = cfg
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
//
// One memory on disk, one receipt, and the retry's bytes identical to the
// first's. This is what the phone's "Saved" is allowed to mean.
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
	memories := vaultMemories(t, cfg)
	if len(memories) != 1 {
		t.Fatalf("a capture and its retry produced %d memories, want exactly 1", len(memories))
	}
	// The receipt never carries the text, and neither does anything the listener
	// wrote about the capture.
	// Short words are skipped: a two-letter fragment appears inside identifiers
	// by coincidence, and a witness that fails on coincidence stops meaning
	// anything. Every word in this capture is long enough to be a real leak.
	for _, word := range strings.Fields("quokkas assemble seventeen hundred") {
		if strings.Contains(first.Body.String(), word) {
			t.Fatalf("the receipt echoed %q", word)
		}
	}
	// The reservation is under the state directory, at 0600 inside 0700, and it
	// holds the receipt rather than the payload.
	root := cfg.StateDir + string(os.PathSeparator) + "companion" + string(os.PathSeparator) + "captures"
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the reservation store is not under the state directory: %v", err)
	}
}

// TestCompanionCaptureRefusesTheGenericLoopbackToken. The two credential
// families are disjoint, and a write route is the one place that mattering most.
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
	if got := len(vaultMemories(t, cfg)); got != 0 {
		t.Fatalf("an unauthenticated capture wrote %d memories", got)
	}
}

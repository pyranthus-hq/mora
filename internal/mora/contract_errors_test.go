package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// contract_errors_test.go pins CON-07: a malformed connector response must be
// distinguishable from unavailable, unauthorized, stale, and empty. The literal
// statement of that requirement is the pairwise-distinctness assertion in
// TestContractConnectorErrorClasses.

// TestContractConnectorErrorClasses is CON-07. Each of the five discriminations
// resolves to its own registered code, and no two of them collide.
//
// MUTATION: point any two rows below at the same code, or make
// connectorCodeForCause return one code for two different causes. The pairwise
// check must fail and name the pair.
func TestContractConnectorErrorClasses(t *testing.T) {
	withTempHome(t)
	registry := loadErrorCodeRegistry(t)
	registered := map[string]errorCodeRow{}
	for _, row := range registry.Codes {
		registered[row.Code] = row
	}

	for _, tc := range []struct {
		name string
		// discrimination is the CON-07 error_class this row must produce.
		discrimination string
		// produce returns the error whose code is under test. A nil produce means
		// the code has no live producer yet and only its registration is asserted.
		produce  func(t *testing.T) error
		wantCode string
	}{
		{
			name:           "malformed: a connector returned a payload Mora cannot decode",
			discrimination: connectorClassMalformed,
			produce: func(t *testing.T) error {
				var payload struct{ N int }
				decodeErr := json.Unmarshal([]byte(`{"n": "not a number"}`), &payload)
				if decodeErr == nil {
					t.Fatal("the synthetic payload must fail to decode")
				}
				return classifyConnectorError(fmt.Errorf("github issues: %w", decodeErr))
			},
			wantCode: errCodeConnectorMalformed,
		},
		{
			name:           "unavailable: the connector's backing file or process is not there",
			discrimination: connectorClassUnavailable,
			produce: func(t *testing.T) error {
				_, openErr := os.Open(filepath.Join(t.TempDir(), "no-such-chat.db"))
				if openErr == nil {
					t.Fatal("opening a missing file must fail")
				}
				return classifyConnectorError(fmt.Errorf("imessage: %w", openErr))
			},
			wantCode: errCodeConnectorUnavailable,
		},
		{
			name:           "unauthorized: the credential itself could not be loaded",
			discrimination: connectorClassUnauthorized,
			produce: func(t *testing.T) error {
				// The shape ingestGoogle raises when google.LoadToken fails: a
				// directly observed credential failure, not an inference.
				return classifyConnectorError(newCodedError(errCodeConnectorUnauthorized,
					fs.ErrNotExist, "not connected to google (run `mora connect google`): %v", fs.ErrNotExist))
			},
			wantCode: errCodeConnectorUnauthorized,
		},
		{
			name:           "stale: a source last succeeded past its freshness budget",
			discrimination: connectorClassStale,
			produce: func(t *testing.T) error {
				// Staleness is a STATE, not a raised error: it is derived from the
				// persisted record rather than thrown by a connector, so the
				// discrimination is asserted where it is actually produced.
				code := syncErrorCodeForState(healthStale, "", "")
				return newCodedError(code, nil, "source is stale")
			},
			wantCode: errCodeConnectorStale,
		},
		{
			name:           "empty: a clean read that returned zero items",
			discrimination: connectorClassEmpty,
			// No live producer in this phase by design: emitting `empty` on a
			// per-source receipt is Phase 2 (ISO-02) work. The code is defined and
			// registered here so Phase 2 has something to emit, and so the
			// pairwise check below already covers it.
			produce:  nil,
			wantCode: errCodeConnectorEmpty,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, ok := registered[tc.wantCode]
			if !ok {
				t.Fatalf("%s is not registered in eval/error-code-registry.json", tc.wantCode)
			}
			if row.ErrorClass != tc.discrimination {
				t.Fatalf("%s: registry error_class %q, want %q", tc.wantCode, row.ErrorClass, tc.discrimination)
			}
			if got := connectorErrorClassOf(tc.wantCode); got != tc.discrimination {
				t.Fatalf("connectorErrorClassOf(%s) = %q, want %q", tc.wantCode, got, tc.discrimination)
			}
			if tc.produce == nil {
				return
			}
			err := tc.produce(t)
			var typed moraError
			if !errors.As(err, &typed) {
				t.Fatalf("produced error %v carries no moraError", err)
			}
			if typed.Code != tc.wantCode {
				t.Fatalf("produced code %q, want %q", typed.Code, tc.wantCode)
			}
			if typed.Class != errClassConnector {
				t.Fatalf("produced class %q, want %q", typed.Class, errClassConnector)
			}
			if got := connectorErrorCodeFor(err); got != tc.wantCode {
				t.Fatalf("connectorErrorCodeFor = %q, want %q", got, tc.wantCode)
			}
		})
	}

	// CON-07, stated literally: the five discriminations are pairwise distinct,
	// as codes and as error_class values.
	five := []struct{ class, code string }{
		{connectorClassMalformed, errCodeConnectorMalformed},
		{connectorClassUnavailable, errCodeConnectorUnavailable},
		{connectorClassUnauthorized, errCodeConnectorUnauthorized},
		{connectorClassStale, errCodeConnectorStale},
		{connectorClassEmpty, errCodeConnectorEmpty},
	}
	for i := range five {
		for j := i + 1; j < len(five); j++ {
			if five[i].code == five[j].code {
				t.Errorf("CON-07 violated: %s and %s share the code %q",
					five[i].class, five[j].class, five[i].code)
			}
			if five[i].class == five[j].class {
				t.Errorf("CON-07 violated: codes %q and %q share the error_class %q",
					five[i].code, five[j].code, five[i].class)
			}
		}
	}
}

// TestContractConnectorCauseClassificationIsStructural pins that the classifier
// reads error STRUCTURE, never prose. A message that merely says "unauthorized"
// must not be typed as unauthorized — that is the inference habit DOC-03 exists
// to remove, and a typed taxonomy must not smuggle it back in.
func TestContractConnectorCauseClassificationIsStructural(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{"json syntax error", &json.SyntaxError{}, errCodeConnectorMalformed},
		{"json type error", &json.UnmarshalTypeError{Value: "string", Type: nil}, errCodeConnectorMalformed},
		{"truncated payload", io.ErrUnexpectedEOF, errCodeConnectorMalformed},
		{"missing file", fs.ErrNotExist, errCodeConnectorUnavailable},
		{"missing binary", &exec.Error{Name: "gh", Err: exec.ErrNotFound}, errCodeConnectorUnavailable},
		{"deadline", context.DeadlineExceeded, errCodeConnectorUnavailable},
		{"prose claiming unauthorized", errors.New("server said unauthorized: 401"), errCodeConnectorUnclassified},
		{"prose claiming full disk access", errors.New("Full Disk Access not granted?"), errCodeConnectorUnclassified},
		{"opaque failure", errors.New("something went wrong"), errCodeConnectorUnclassified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := connectorCodeForCause(fmt.Errorf("wrapped: %w", tc.cause)); got != tc.want {
				t.Fatalf("connectorCodeForCause(%v) = %q, want %q", tc.cause, got, tc.want)
			}
		})
	}

	// A boundary that already typed itself keeps its own code.
	typedAtSource := newCodedError(errCodeConnectorUnauthorized, nil, "token load failed")
	var got moraError
	if !errors.As(classifyConnectorError(typedAtSource), &got) || got.Code != errCodeConnectorUnauthorized {
		t.Fatal("classifyConnectorError overwrote a code the raising site already set")
	}

	// The message and the cause survive classification untouched: this adds a
	// label, it does not rewrite what the user reads or what errors.Is sees.
	cause := fs.ErrNotExist
	original := fmt.Errorf("cannot read your Messages database (Full Disk Access not granted?): %w", cause)
	classified := classifyConnectorError(original)
	if classified.Error() != original.Error() {
		t.Fatalf("classification changed the message:\n got %q\nwant %q", classified.Error(), original.Error())
	}
	if !errors.Is(classified, cause) {
		t.Fatal("classification hid the cause from errors.Is")
	}
}

// TestContractSQLiteErrorsCarryIndexCodes pins the sqlite half: the driver's
// prose is still the only signal, but it now resolves to a published code in one
// named place.
func TestContractSQLiteErrorsCarryIndexCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"duplicate column", errors.New("SQL logic error: duplicate column name: provider (1)"), errCodeIndexSchemaMismatch},
		{"no such table", errors.New("SQL logic error: no such table: index_meta (1)"), errCodeIndexSchemaMismatch},
		{"no such column", errors.New("SQL logic error: no such column: salience_micros (1)"), errCodeIndexSchemaMismatch},
		{"unopenable file", errors.New("unable to open database file: no such file or directory"), errCodeIndexUnavailable},
		{"missing file sentinel", fmt.Errorf("open index: %w", fs.ErrNotExist), errCodeIndexUnavailable},
		{"unexplained", errors.New("disk I/O error (10)"), errCodeInternalUnexpected},
		{"nil", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sqliteErrorCode(tc.err); got != tc.want {
				t.Fatalf("sqliteErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestContractSyncStatusErrorCodeIsAdditive proves the persisted record gained a
// typed companion WITHOUT retyping the prose field, and that a file written
// before the field existed still decodes.
func TestContractSyncStatusErrorCodeIsAdditive(t *testing.T) {
	// A record persisted before this change: no error_code key at all.
	const historic = `{
	  "source": "gmail",
	  "last_synced": "2026-08-01T00:00:00Z",
	  "item_count": 12,
	  "error_count": 1,
	  "last_error": "database or disk is full (13)",
	  "last_attempt_at": "2026-08-01T00:00:00Z",
	  "last_success_at": "2026-07-30T00:00:00Z"
	}`
	var st memory.SyncStatus
	if err := json.Unmarshal([]byte(historic), &st); err != nil {
		t.Fatalf("a pre-taxonomy sync record must still decode: %v", err)
	}
	if st.ErrorCode != "" {
		t.Fatalf("ErrorCode = %q on a record that never had the field, want empty", st.ErrorCode)
	}
	if st.LastError != "database or disk is full (13)" {
		t.Fatalf("LastError was not preserved: %q", st.LastError)
	}
	// Mora reads that empty slot as unclassified rather than rewriting the file.
	if got := syncErrorCodeForState(healthFailed, st.ErrorCode, st.LastError); got != errCodeConnectorUnclassified {
		t.Fatalf("historic failure reads as %q, want %q", got, errCodeConnectorUnclassified)
	}
	// Round-tripping a record with no code must not introduce the key.
	encoded, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("error_code")) {
		t.Fatalf("omitempty broken: an empty ErrorCode was serialized: %s", encoded)
	}
}

// TestContractSyncStatusReceiptCarriesErrorCode drives the real CLI: a persisted
// failure must surface a typed code in `mora sync status --json`, which is the
// slot Plan 01-05 shaped and left empty for exactly this.
func TestContractSyncStatusReceiptCarriesErrorCode(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	for _, rec := range []struct {
		file string
		st   memory.SyncStatus
	}{
		{
			// A failure typed by this taxonomy.
			file: "google-gmail.json",
			st: memory.SyncStatus{
				Source: "gmail", ItemCount: 3, ErrorCount: 1,
				LastError:     "not connected to google",
				ErrorCode:     errCodeConnectorUnauthorized,
				LastAttemptAt: now.Format(time.RFC3339),
				LastSuccessAt: now.Add(-time.Hour).Format(time.RFC3339),
			},
		},
		{
			// A failure persisted before the taxonomy: no code on disk.
			file: "imessage-imsg.json",
			st: memory.SyncStatus{
				Source: "imsg", ItemCount: 7, ErrorCount: 2,
				LastError:     "database or disk is full (13)",
				LastAttemptAt: now.Format(time.RFC3339),
				LastSuccessAt: now.Add(-time.Hour).Format(time.RFC3339),
			},
		},
		{
			// A clean but aging source: stale is a state, and it carries the
			// CON-07 discrimination that state expresses.
			file: "github-gh.json",
			st: memory.SyncStatus{
				Source: "gh", ItemCount: 40,
				LastAttemptAt: now.Add(-72 * time.Hour).Format(time.RFC3339),
				LastSuccessAt: now.Add(-72 * time.Hour).Format(time.RFC3339),
			},
		},
		{
			// A healthy source carries no code at all.
			file: "filesystem-docs.json",
			st: memory.SyncStatus{
				Source: "docs", ItemCount: 5,
				LastAttemptAt: now.Format(time.RFC3339),
				LastSuccessAt: now.Format(time.RFC3339),
			},
		},
	} {
		path := filepath.Join(cfg.StateDir, "sync", rec.file)
		if err := memory.SaveStatus(path, &rec.st); err != nil {
			t.Fatal(err)
		}
	}

	stdout, _, err := runSplit(t, "sync", "status", "--json")
	if err != nil {
		t.Fatalf("sync status --json: %v", err)
	}
	var receipt struct {
		Schema  string `json:"schema"`
		Sources []struct {
			Source    string `json:"source"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			ErrorCode string `json:"error_code"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
		t.Fatalf("decode sync status receipt: %v\n%s", err, stdout)
	}
	got := map[string]string{}
	for _, s := range receipt.Sources {
		got[s.Source] = s.ErrorCode
	}
	want := map[string]string{
		"gmail": errCodeConnectorUnauthorized,
		"imsg":  errCodeConnectorUnclassified,
		"gh":    errCodeConnectorStale,
		"docs":  "",
	}
	for source, wantCode := range want {
		if got[source] != wantCode {
			t.Errorf("sync status error_code for %q = %q, want %q\n%s", source, got[source], wantCode, stdout)
		}
	}

	// Every non-empty code the receipt publishes is a registered one.
	registry := loadErrorCodeRegistry(t)
	registered := map[string]bool{}
	for _, row := range registry.Codes {
		registered[row.Code] = true
	}
	for source, code := range got {
		if code != "" && !registered[code] {
			t.Errorf("sync status published unregistered code %q for %q", code, source)
		}
	}
}

// TestContractStampedFailureCarriesErrorCode proves the code lands on DISK, not
// just in the payload: stampSyncAttemptFailure is the single boundary every
// connector failure is persisted through.
func TestContractStampedFailureCarriesErrorCode(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Name: "gmail", Type: "gmail"}
	path := syncStatusPathFor(cfg, source)
	if path == "" {
		t.Fatal("no status path for the test source")
	}

	failure := classifyConnectorError(fmt.Errorf("imessage: %w", fs.ErrNotExist))
	stampSyncAttemptFailure(cfg, source, failure, time.Now(), io.Discard)

	st, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.ErrorCode != errCodeConnectorUnavailable {
		t.Fatalf("persisted ErrorCode = %q, want %q", st.ErrorCode, errCodeConnectorUnavailable)
	}
	if st.LastError != failure.Error() {
		t.Fatalf("persisted LastError = %q, want the unchanged prose %q", st.LastError, failure.Error())
	}

	// And it reaches the derived health snapshot the doctor report publishes.
	h := sourceHealthFor(cfg, source, "gmail", time.Now())
	if h.ErrorCode != errCodeConnectorUnavailable {
		t.Fatalf("sourceHealth.ErrorCode = %q, want %q", h.ErrorCode, errCodeConnectorUnavailable)
	}
	if h.State != healthNever {
		// No prior success was ever recorded in this test, so the state is
		// `never` — the code is what tells a machine WHY, which is the point.
		t.Logf("state = %q (no prior success recorded)", h.State)
	}
}

// stubFetcher drives memory.Ingest without a live connector, so a test can
// reproduce a failure raised INSIDE the shared ingest loop — the family whose
// LastAttemptAt stamp trips stampSyncAttemptFailure's inner-path guard.
type stubFetcher struct {
	page memory.Page
	err  error
}

func (f stubFetcher) FetchPage(kind memory.ItemKind, w memory.FetchWindow, cursor string) (memory.Page, error) {
	if f.err != nil {
		return memory.Page{}, f.err
	}
	return f.page, nil
}

// TestContractInnerPathFailureCarriesErrorCode (01-06 review, P1 #1): a failure
// raised inside memory.Ingest stamps its own LastAttemptAt, so the outer
// stampSyncAttemptFailure correctly declines to re-stamp. That guard is load
// bearing and must stay. What it means is that the outer stamp is NOT the only
// place a connector failure is persisted — persistSyncStatus must type this
// family, or the whole dropped-item/fetch-failure family lands on disk with
// prose and no code.
//
// MUTATION: remove the `if ingErr != nil { st.ErrorCode = ... }` block from
// persistSyncStatus. This test must fail.
func TestContractInnerPathFailureCarriesErrorCode(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Name: "gmail", Type: "gmail"}
	path := syncStatusPathFor(cfg, source)

	// A prior, DIFFERENT failure already typed this record.
	if err := memory.SaveStatus(path, &memory.SyncStatus{
		Source: "gmail", ErrorCount: 1,
		LastError: "not connected to google", ErrorCode: errCodeConnectorUnauthorized,
	}); err != nil {
		t.Fatal(err)
	}
	st, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatal(err)
	}

	// ingestSource captures attemptStart BEFORE dispatch; memory.Ingest stamps
	// LastAttemptAt after it, which is precisely what trips the guard.
	attemptStart := time.Now()
	res, ingErr := memory.Ingest(memory.IngestParams{
		Fetcher: stubFetcher{err: fmt.Errorf("fetch page: %w", fs.ErrNotExist)},
		Kind:    "gmail_thread", Scope: "personal", BodyBudget: 1000,
		Status: st, Write: func(memory.MappedMemory) error { return nil },
	})
	if ingErr == nil {
		t.Fatal("the stub fetcher must fail")
	}
	if perr := persistSyncStatus(io.Discard, path, res.Status, ingErr); perr == nil {
		t.Fatal("persistSyncStatus must return the ingest error")
	}

	// Now the outer boundary runs, exactly as ingestSource drives it.
	stampSyncAttemptFailure(cfg, source, classifyConnectorError(ingErr), attemptStart, io.Discard)

	got, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ErrorCode != errCodeConnectorUnavailable {
		t.Fatalf("persisted ErrorCode = %q, want %q — a failure raised inside memory.Ingest was left untyped or kept a stale code",
			got.ErrorCode, errCodeConnectorUnavailable)
	}
	// The guard's own contract still holds: the inner path's prose survives.
	if got.LastError != ingErr.Error() {
		t.Fatalf("LastError = %q, want the inner path's own prose %q", got.LastError, ingErr.Error())
	}
}

// TestContractRecoveredSourceReportsNoErrorCode (01-06 review, P1 #2): the whole
// point of a typed receipt is that an agent can act on it. A `fresh` source that
// still reports connector.unauthorized inverts that. Drives failure -> clean
// recovery end to end and asserts the code is gone from the record, from the
// derived health snapshot, and from the published receipt.
//
// MUTATION: drop `p.Status.ErrorCode = ""` from memory.Ingest's clean-completion
// reset. This test must fail three times over.
func TestContractRecoveredSourceReportsNoErrorCode(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Name: "gmail", Type: "gmail"}
	path := syncStatusPathFor(cfg, source)

	// The source failed with a typed code on a prior run.
	if err := memory.SaveStatus(path, &memory.SyncStatus{
		Source: "gmail", ItemCount: 4, ErrorCount: 1,
		LastError: "not connected to google", ErrorCode: errCodeConnectorUnauthorized,
		LastAttemptAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		LastSuccessAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	st, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.ErrorCode != errCodeConnectorUnauthorized {
		t.Fatalf("setup: seeded code did not persist, got %q", st.ErrorCode)
	}

	// It recovers: one clean page, nothing dropped.
	res, ingErr := memory.Ingest(memory.IngestParams{
		Fetcher: stubFetcher{page: memory.Page{}},
		Kind:    "gmail_thread", Scope: "personal", BodyBudget: 1000,
		Status: st, Write: func(memory.MappedMemory) error { return nil },
	})
	if ingErr != nil {
		t.Fatalf("the clean run must succeed: %v", ingErr)
	}
	if perr := persistSyncStatus(io.Discard, path, res.Status, nil); perr != nil {
		t.Fatal(perr)
	}

	// 1. The persisted record.
	got, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ErrorCode != "" {
		t.Errorf("recovered source still carries error_code %q on disk", got.ErrorCode)
	}
	if got.LastError != "" || got.ErrorCount != 0 {
		t.Errorf("the prose reset regressed: LastError=%q ErrorCount=%d", got.LastError, got.ErrorCount)
	}

	// 2. The derived health snapshot doctor publishes.
	h := sourceHealthFor(cfg, source, "gmail", time.Now())
	if h.State != healthFresh {
		t.Fatalf("recovered source state = %q, want %q", h.State, healthFresh)
	}
	if h.ErrorCode != "" {
		t.Errorf("a %s source reports error_code %q — the receipt says a healthy source is broken",
			healthFresh, h.ErrorCode)
	}

	// 3. The published receipt an agent actually reads.
	stdout, _, err := runSplit(t, "sync", "status", "--json")
	if err != nil {
		t.Fatalf("sync status --json: %v", err)
	}
	var receipt struct {
		Sources []struct {
			Source    string `json:"source"`
			State     string `json:"state"`
			ErrorCode string `json:"error_code"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
		t.Fatalf("decode receipt: %v\n%s", err, stdout)
	}
	found := false
	for _, s := range receipt.Sources {
		if s.Source != "gmail" {
			continue
		}
		found = true
		if s.State != healthFresh {
			t.Errorf("receipt state = %q, want %q", s.State, healthFresh)
		}
		if s.ErrorCode != "" {
			t.Errorf("receipt error_code = %q on a recovered source", s.ErrorCode)
		}
	}
	if !found {
		t.Fatalf("gmail missing from the receipt:\n%s", stdout)
	}
}

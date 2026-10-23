package eywa

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

var fixedNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func ingestAll(t *testing.T, f *Fetcher, st *memory.SyncStatus) ([]memory.MappedMemory, error) {
	t.Helper()
	var written []memory.MappedMemory
	_, err := memory.Ingest(memory.IngestParams{
		Fetcher: f, Kind: KindFact, Scope: "global", Status: st, Map: MapFact,
		WriteResult: func(mm memory.MappedMemory) (bool, error) {
			if msg, ok := mm.Meta["eywa_record_error"].(string); ok {
				return false, errors.New(msg)
			}
			written = append(written, mm)
			return true, nil
		},
	})
	return written, err
}

func byID(mms []memory.MappedMemory) map[string]memory.MappedMemory {
	out := map[string]memory.MappedMemory{}
	for _, m := range mms {
		out[m.StableID] = m
	}
	return out
}

func TestValidateEndpoints(t *testing.T) {
	cases := []struct {
		endpoint, token, client string
		ok                      bool
	}{
		{"https://eywa.example.com/mcp", "https://auth.example.com/oauth2/token", "c", true},
		{"http://127.0.0.1:8080/mcp", "http://localhost:9/token", "c", true},
		{"http://eywa.example.com/mcp", "https://auth.example.com/token", "c", false},
		{"https://user:pw@eywa.example.com/mcp", "https://auth.example.com/token", "c", false},
		{"", "https://auth.example.com/token", "c", false},
		{"https://eywa.example.com/mcp", "https://auth.example.com/token", "", false},
		{"ftp://eywa.example.com", "https://auth.example.com/token", "c", false},
	}
	for _, c := range cases {
		if err := ValidateEndpoints(c.endpoint, c.token, c.client); (err == nil) != c.ok {
			t.Errorf("ValidateEndpoints(%q, %q, %q) = %v, want ok=%v", c.endpoint, c.token, c.client, err, c.ok)
		}
	}
}

func TestNewClientRequiresSecretFromEnvironment(t *testing.T) {
	cfg := Config{Endpoint: "https://eywa.example.com/mcp", TokenURL: "https://auth.example.com/token", ClientID: "c"}
	_, err := NewClient(cfg, ClientOptions{})
	if err == nil || !strings.Contains(err.Error(), SecretEnv) {
		t.Fatalf("NewClient without secret = %v, want a pointer to %s", err, SecretEnv)
	}
}

func TestPullCursorAndReplacedVersions(t *testing.T) {
	f := newFakeService(t)
	f.addVersion("fct_1", "ver_A", "Acme Corp", "payment terms", "net 30", "current", "2026-08-01T00:00:00Z", "")
	f.pages = []fakePage{
		{items: []map[string]any{{"change_id": "e1", "type": "fact.admitted", "ids": map[string]any{"fact_id": "fct_1", "version_id": "ver_A"}}}, next: "c1", hasMore: true},
		{items: []map[string]any{{"change_id": "e2", "type": "write.status", "ids": map[string]any{"write_id": "wrt_x"}, "status": "received", "own": true}}, next: "c2"},
	}
	fetcher := NewFetcher(f.client(t), nil, func() time.Time { return fixedNow })
	st := &memory.SyncStatus{}
	got, err := ingestAll(t, fetcher, st)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if st.IncrementalCursor != "c2" {
		t.Fatalf("cursor after first sync = %q, want c2", st.IncrementalCursor)
	}
	first := f.toolCalls("changes_since")
	if len(first) != 2 || first[0].Args["since"] == nil || first[1].Args["cursor"] != "c1" {
		t.Fatalf("changes_since calls = %+v, want since then cursor c1", first)
	}
	if since, _ := time.Parse(time.RFC3339, first[0].Args["since"].(string)); fixedNow.Sub(since) > MaxLookback {
		t.Fatalf("first since %v is beyond the 35-day horizon", since)
	}
	m, ok := byID(got)["eywa_fact/ver_A"]
	if !ok {
		t.Fatalf("records = %v, want eywa_fact/ver_A", got)
	}
	if m.Provider != "eywa" || m.Type != "fact" || m.Meta["status"] != "current" || m.Meta["source_uri"] != "https://wiki.example.com/ver_A" {
		t.Fatalf("record = %+v", m)
	}
	if !strings.Contains(m.Body, "Citation: https://wiki.example.com/ver_A @ ver_A") || !strings.Contains(m.Body, "> net 30 per the wiki") {
		t.Fatalf("body lacks citation or excerpt:\n%s", m.Body)
	}
	if fetcher.Stats.OwnUpdates != 1 {
		t.Fatalf("own updates = %d, want 1", fetcher.Stats.OwnUpdates)
	}

	// The fact is replaced. The next sync resumes from the stored cursor, sees
	// only the replacement by id, resolves it through what the vault knows, and
	// marks the old version replaced instead of dropping it.
	f.retire("Acme Corp", "payment terms", "ver_A", "2026-08-20T00:00:00Z")
	f.addVersion("fct_2", "ver_B", "Acme Corp", "payment terms", "net 45", "current", "2026-08-20T00:00:00Z", "")
	f.pages = append(f.pages, fakePage{items: []map[string]any{{"change_id": "e3", "type": "version.superseded", "ids": map[string]any{"old_version_id": "ver_A", "new_version_id": "ver_B"}}}, next: "c3"})
	known := map[string]Known{}
	for _, mm := range got {
		if id, k, ok := KnownFromMeta(mm.Meta); ok {
			known[id] = k
		}
	}
	fetcher2 := NewFetcher(f.client(t), func(id string) (Known, bool) { k, ok := known[id]; return k, ok }, func() time.Time { return fixedNow })
	got2, err := ingestAll(t, fetcher2, st)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	calls := f.toolCalls("changes_since")
	if last := calls[len(calls)-1]; last.Args["cursor"] != "c2" || last.Args["since"] != nil {
		t.Fatalf("second sync did not resume from the stored cursor: %+v", last.Args)
	}
	if st.IncrementalCursor != "c3" {
		t.Fatalf("cursor after second sync = %q, want c3", st.IncrementalCursor)
	}
	recs := byID(got2)
	old, okA := recs["eywa_fact/ver_A"]
	cur, okB := recs["eywa_fact/ver_B"]
	if !okA || !okB {
		t.Fatalf("second sync records = %v, want ver_A (replaced) and ver_B (current)", recs)
	}
	if old.Meta["status"] != "replaced" || old.Meta["replaced_by"] != "ver_B" || old.Meta["retired_at"] != "2026-08-20T00:00:00Z" {
		t.Fatalf("old version not marked replaced: %+v", old.Meta)
	}
	if old.Meta["excerpt"] != "net 30 per the wiki" || old.Meta["content_hash"] == nil {
		t.Fatalf("replaced version lost lineage it had: %+v", old.Meta)
	}
	if old.DeletedAt != "" {
		t.Fatal("a replaced version must never be tombstoned")
	}
	if cur.Meta["status"] != "current" || cur.Meta["value"] != "net 45" {
		t.Fatalf("new version = %+v", cur.Meta)
	}
}

func TestPullReplacementOfUnknownVersionIsSkippedAndCounted(t *testing.T) {
	f := newFakeService(t)
	f.pages = []fakePage{{items: []map[string]any{{"type": "version.superseded", "ids": map[string]any{"old_version_id": "ver_X", "new_version_id": "ver_Y"}}}, next: "c1"}}
	fetcher := NewFetcher(f.client(t), nil, func() time.Time { return fixedNow })
	got, err := ingestAll(t, fetcher, &memory.SyncStatus{})
	if err != nil || len(got) != 0 || fetcher.Stats.Skipped != 1 {
		t.Fatalf("got=%v err=%v skipped=%d", got, err, fetcher.Stats.Skipped)
	}
}

func TestPullPerRecordFailureIsSurfaced(t *testing.T) {
	f := newFakeService(t)
	f.addVersion("fct_1", "ver_A", "Acme Corp", "owner", "Riya", "current", "2026-08-01T00:00:00Z", "")
	f.pages = []fakePage{{items: []map[string]any{
		{"type": "fact.admitted", "ids": map[string]any{"fact_id": "fct_gone", "version_id": "ver_gone"}},
		{"type": "fact.admitted", "ids": map[string]any{"fact_id": "fct_1", "version_id": "ver_A"}},
	}, next: "c1"}}
	fetcher := NewFetcher(f.client(t), nil, func() time.Time { return fixedNow })
	st := &memory.SyncStatus{}
	got, err := ingestAll(t, fetcher, st)
	if err == nil || !strings.Contains(err.Error(), "1 item(s) failed") {
		t.Fatalf("err = %v, want one dropped record surfaced", err)
	}
	if !strings.Contains(st.LastError, "fct_gone") || len(got) != 1 {
		t.Fatalf("last error %q, records %d", st.LastError, len(got))
	}
}

func TestPullAuthFailuresAreTyped(t *testing.T) {
	t.Run("token endpoint 401", func(t *testing.T) {
		f := newFakeService(t)
		f.tokenCode = http.StatusUnauthorized
		_, err := NewFetcher(f.client(t), nil, nil).FetchPage(KindFact, memory.FetchWindow{}, "")
		var ae *AuthError
		if !errors.As(err, &ae) || ae.Stage != "token" || !ae.ConnectorUnauthorized() {
			t.Fatalf("err = %v, want a token-stage AuthError", err)
		}
		if strings.Contains(err.Error(), "secret-a") {
			t.Fatal("error leaks the client secret")
		}
	})
	t.Run("mcp 403", func(t *testing.T) {
		f := newFakeService(t)
		f.mcpStatus["changes_since"] = http.StatusForbidden
		_, err := NewFetcher(f.client(t), nil, nil).FetchPage(KindFact, memory.FetchWindow{}, "")
		var ae *AuthError
		if !errors.As(err, &ae) || ae.Status != 403 {
			t.Fatalf("err = %v, want AuthError 403", err)
		}
	})
	t.Run("json-rpc forbidden on the feed is an agent failure", func(t *testing.T) {
		f := newFakeService(t)
		f.rpcErr["changes_since"] = rpcErrorBody{Code: -32001, Message: "Forbidden"}
		_, err := NewFetcher(f.client(t), nil, nil).FetchPage(KindFact, memory.FetchWindow{}, "")
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("err = %v, want AuthError", err)
		}
	})
	t.Run("401 refreshes the token once", func(t *testing.T) {
		f := newFakeService(t)
		f.mcpStatus["changes_since"] = http.StatusUnauthorized
		if _, err := NewFetcher(f.client(t), nil, nil).FetchPage(KindFact, memory.FetchWindow{}, ""); err != nil {
			t.Fatalf("a single 401 should refresh and succeed: %v", err)
		}
		if f.tokens != 2 {
			t.Fatalf("tokens minted = %d, want 2", f.tokens)
		}
	})
}

func TestRateLimitHonorsRetryAfter(t *testing.T) {
	f := newFakeService(t)
	f.mcpStatus["changes_since"] = http.StatusTooManyRequests
	f.retryAfter = "7"
	var slept []time.Duration
	c, err := NewClient(f.config(), ClientOptions{HTTP: f.srv.Client(), Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFetcher(c, nil, nil).FetchPage(KindFact, memory.FetchWindow{}, ""); err != nil {
		t.Fatalf("a short Retry-After should be waited out: %v", err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Fatalf("slept %v, want [7s]", slept)
	}

	// A Retry-After beyond the bounded wait is reported, not slept through.
	f.mcpStatus["changes_since"] = http.StatusTooManyRequests
	f.retryAfter = "600"
	_, err = NewFetcher(c, nil, nil).FetchPage(KindFact, memory.FetchWindow{}, "")
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 600*time.Second || !strings.Contains(err.Error(), "retry after 10m0s") {
		t.Fatalf("err = %v, want RateLimitError 600s", err)
	}

	// JSON-RPC Throttled carries retry_after in error.data.
	f.rpcErr["changes_since"] = rpcErrorBody{Code: -32002, Message: "Throttled", Data: []byte(`{"retry_after":900}`)}
	_, err = NewFetcher(c, nil, nil).FetchPage(KindFact, memory.FetchWindow{}, "")
	if !errors.As(err, &rl) || rl.RetryAfter != 900*time.Second {
		t.Fatalf("err = %v, want throttled 900s", err)
	}
}

func TestPullRejectedCursorFallsBackToFreshWalk(t *testing.T) {
	f := newFakeService(t)
	f.pages = []fakePage{{next: "c1"}}
	fetcher := NewFetcher(f.client(t), nil, func() time.Time { return fixedNow })
	st := &memory.SyncStatus{IncrementalCursor: "bogus"}
	if _, err := ingestAll(t, fetcher, st); err != nil {
		t.Fatalf("expired cursor should fall back: %v", err)
	}
	if st.IncrementalCursor != "c1" {
		t.Fatalf("cursor = %q, want c1", st.IncrementalCursor)
	}
}

func TestEventStreamResponsesAndSessionRecovery(t *testing.T) {
	f := newFakeService(t)
	f.sse = true
	f.pages = []fakePage{{next: "c1"}}
	c := f.client(t)
	if _, err := NewFetcher(c, nil, nil).FetchPage(KindFact, memory.FetchWindow{}, ""); err != nil {
		t.Fatalf("event-stream response: %v", err)
	}
	f.session = "sess-2" // the server forgets the old session
	if _, err := NewFetcher(c, nil, nil).FetchPage(KindFact, memory.FetchWindow{}, ""); err != nil {
		t.Fatalf("session recovery: %v", err)
	}
}

func TestBuildUpdateLineage(t *testing.T) {
	u, err := BuildUpdate("agt_TEST", "mem_01ABC", "Acme payment terms", "Acme Corp pays on net 45 from September.")
	if err != nil {
		t.Fatal(err)
	}
	if u.SourceURI != "eywa-run://agt_TEST/mora/mem_01ABC" {
		t.Fatalf("source uri = %q", u.SourceURI)
	}
	if u.Excerpt != u.Text || u.ContentHash != ContentHash(u.Text) || len(u.ContentHash) != 64 {
		t.Fatalf("lineage does not cover the text: %+v", u)
	}
	// NFC + trim: a decomposed é hashes like the composed one.
	if ContentHash("  Café owner\n") != ContentHash("Café owner") {
		t.Fatal("content hash is not canonical (NFC, trimmed)")
	}
	if _, err := BuildUpdate("agt_TEST", "mem_big", "", strings.Repeat("x", MaxUpdateChars+1)); err == nil {
		t.Fatal("an over-long memory must be refused, not trimmed")
	}
	if _, err := BuildUpdate("", "mem_1", "", "text"); err == nil {
		t.Fatal("push without an agent id must be refused")
	}
}

func TestRememberSendsLineageAndPollsInReview(t *testing.T) {
	f := newFakeService(t)
	c := f.client(t)
	u, err := BuildUpdate("agt_TEST", "mem_01ABC", "", "Acme Corp pays on net 45.")
	if err != nil {
		t.Fatal(err)
	}
	ack, err := c.Remember(context.Background(), "payments", u)
	if err != nil {
		t.Fatal(err)
	}
	if ack.WriteID != "wrt_01" || ack.Status != "received" {
		t.Fatalf("ack = %+v", ack)
	}
	sent := f.remembered[0]
	lin := sent["lineage"].(map[string]any)
	if sent["topic"] != "payments" || sent["text"] != u.Text || lin["source_uri"] != u.SourceURI || lin["content_hash"] != u.ContentHash || lin["excerpt"] != u.Text || sent["idempotency_key"] == "" {
		t.Fatalf("remember args = %+v", sent)
	}
	if _, hasSigner := lin["signer"]; hasSigner {
		t.Fatal("push must not claim a signer; the token identity is the writer")
	}
	ws, err := c.GetWriteStatus(context.Background(), ack.WriteID)
	if err != nil || ws.Status != "in_review" || !FinalStatus(ws.Status) {
		t.Fatalf("status = %+v err=%v", ws, err)
	}
	if FinalStatus("received") {
		t.Fatal("received is pending, not final")
	}
}

func TestRememberGuardrailIsToolError(t *testing.T) {
	f := newFakeService(t)
	f.toolErr["remember"] = "GuardrailBlocked"
	u, _ := BuildUpdate("agt_TEST", "mem_1", "", "text")
	_, err := f.client(t).Remember(context.Background(), "payments", u)
	var te *ToolError
	if !errors.As(err, &te) || te.Code != "GuardrailBlocked" {
		t.Fatalf("err = %v, want GuardrailBlocked tool error", err)
	}
}

func TestRememberForbiddenTopicIsAuthError(t *testing.T) {
	f := newFakeService(t)
	f.rpcErr["remember"] = rpcErrorBody{Code: -32001, Message: "Forbidden"}
	u, _ := BuildUpdate("agt_TEST", "mem_1", "", "text")
	_, err := f.client(t).Remember(context.Background(), "payments", u)
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("err = %v, want AuthError 403", err)
	}
}

func TestPullReplacementOnTheSamePageAsItsAdmission(t *testing.T) {
	f := newFakeService(t)
	f.addVersion("fct_1", "ver_A", "Billing service", "owner", "Team Red", "current", "2026-08-01T00:00:00Z", "")
	f.retire("Billing service", "owner", "ver_A", "2026-08-02T00:00:00Z")
	f.addVersion("fct_2", "ver_B", "Billing service", "owner", "Team Blue", "current", "2026-08-02T00:00:00Z", "")
	f.pages = []fakePage{{items: []map[string]any{
		{"type": "fact.admitted", "ids": map[string]any{"fact_id": "fct_2", "version_id": "ver_B"}},
		{"type": "version.superseded", "ids": map[string]any{"old_version_id": "ver_A", "new_version_id": "ver_B"}},
	}, next: "c1"}}
	fetcher := NewFetcher(f.client(t), nil, func() time.Time { return fixedNow })
	got, err := ingestAll(t, fetcher, &memory.SyncStatus{})
	if err != nil {
		t.Fatal(err)
	}
	recs := byID(got)
	if recs["eywa_fact/ver_A"].Meta["replaced_by"] != "ver_B" || recs["eywa_fact/ver_B"].Meta["status"] != "current" {
		t.Fatalf("records = %+v", recs)
	}
	if fetcher.Stats.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0: the replacement names a version resolved on the same page", fetcher.Stats.Skipped)
	}
	if n := len(f.toolCalls("history")); n != 1 {
		t.Fatalf("history calls = %d, want one per fact slot", n)
	}
}

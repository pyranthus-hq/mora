package mora

// These are the CLI-and-kernel witnesses for graph node N12. The listener's own
// properties — the route allowlist, the opaque 401, the loopback bind, the
// bounds, the silence — are proved in internal/companion/server_test.go against
// the package that owns them. What is proved here is the half this package
// owns: that the two token families are disjoint, that the generic loopback API
// did not move, that the projections carry the kernel's real freshness, and that
// the subcommand behaves like a server verb.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/companion"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	healthpkg "github.com/pyranthus-hq/mora/internal/health"
	loopbackhttp "github.com/pyranthus-hq/mora/internal/loopbackhttp"
)

// companionTestListener builds the real thing: the real registry over the test
// home, the real kernel reader over the test Config, and a live device token.
func companionTestListener(t *testing.T) (http.Handler, string, Config) {
	t.Helper()
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
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
	// N21 made the governed writer and the reservation store required: the
	// allowlist declares a capture route, and a listener assembled without them
	// would publish a route it cannot serve. This helper hands over the REAL
	// writer, so the tests below that drive the read routes are still driving a
	// production assembly.
	srv, err := companion.NewServer(companion.ServerOptions{
		Addr:     fmt.Sprintf("%s:%d", companion.LoopbackHost, defaultCompanionPort),
		Devices:  reg,
		Pairings: reg,
		Reader:   newCompanionReader(cfg),
		Writer:   newCompanionWriter(),
		Captures: companion.NewReservationStore(cfg.StateDir, companion.WithReservationClock(cfg.OperationClock)),
		Now:      cfg.OperationClock,
	})
	if err != nil {
		t.Fatalf("new companion server: %v", err)
	}
	return srv.Handler(), token, cfg
}

func companionRequest(t *testing.T, method, path, token string, body string) *http.Request {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = fmt.Sprintf("%s:%d", companion.LoopbackHost, defaultCompanionPort)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// companionDerivedID is the shape an evidence citation must have on the wire:
// the published prefix plus a fixed-width one-way digest, and nothing a
// provider id could survive inside.
var companionDerivedID = regexp.MustCompile(`^mem_[0-9a-f]{32}$`)

const companionContextRequestBody = `{"schema":"mora.companion.context.request","schema_version":1,"mode":"search","query":"launch"}`

// ---------------------------------------------------------------------------
// The two token families are disjoint
// ---------------------------------------------------------------------------

// TestCompanionListenerRefusesTheGenericLoopbackToken is half of the
// cross-credential gate.
//
// The loopback token lives in ~/.config/mora/http.json, readable by anything
// running as the user and embedded in a web page the AI browser loads. If it
// opened the companion routes, every one of those readers would hold a phone's
// credential, and revoking a device would not take it away.
func TestCompanionListenerRefusesTheGenericLoopbackToken(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	handler, deviceToken, cfg := companionTestListener(t)

	loopbackToken, err := loopbackhttp.LoadOrCreateToken(cfg.ConfigDir)
	if err != nil {
		t.Fatalf("load loopback token: %v", err)
	}
	if loopbackToken == "" || loopbackToken == deviceToken {
		t.Fatalf("the loopback token and the device token are not distinct")
	}

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, companion.RouteToday, ""},
		{http.MethodGet, companion.RouteHealth, ""},
		{http.MethodPost, companion.RouteContext, companionContextRequestBody},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, companionRequest(t, tc.method, tc.path, loopbackToken, tc.body))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("the generic loopback token got %d on %s, want 401\n%s", rec.Code, tc.path, rec.Body.String())
			}
			// And the device token still works, so the refusal is the
			// credential's doing rather than a broken route.
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, companionRequest(t, tc.method, tc.path, deviceToken, tc.body))
			if rec.Code != http.StatusOK {
				t.Fatalf("the device token got %d on %s, want 200\n%s", rec.Code, tc.path, rec.Body.String())
			}
		})
	}
}

// TestGenericLoopbackAPIRefusesADeviceToken is the other half. A stolen phone
// token must not reach /call, /write or the brief.
func TestGenericLoopbackAPIRefusesADeviceToken(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	_, deviceToken, cfg := companionTestListener(t)

	loopbackToken, err := loopbackhttp.LoadOrCreateToken(cfg.ConfigDir)
	if err != nil {
		t.Fatalf("load loopback token: %v", err)
	}
	generic := (&httpServer{token: loopbackToken, port: 7777}).handler(testCtx(t))

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/call", `{"name":"search_memory","arguments":{"query":"x"}}`},
		{http.MethodPost, "/write", `{"title":"t","text":"x"}`},
		{http.MethodPost, "/search", `{"q":"x"}`},
		{http.MethodPost, "/think", `{"q":"x"}`},
		{http.MethodGet, "/brief", ""},
		{http.MethodGet, "/entity/Sam", ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Host = "127.0.0.1:7777"
			req.Header.Set("Authorization", "Bearer "+deviceToken)
			rec := httptest.NewRecorder()
			generic.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("a device token got %d on the generic %s, want 401\n%s", rec.Code, tc.path, rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The generic API did not move
// ---------------------------------------------------------------------------

// TestLoopbackHTTPResponsesAreByteForByteUnchanged is the no-collateral-damage
// gate for N12.
//
// The companion listener is a SEPARATE server precisely so the generic one does
// not have to change, and the cheapest way for that claim to rot is a shared
// helper edited "harmlessly". These are the exact bytes, byte for byte: the
// landing page an AI browser reads its token from, the unauthenticated refusal,
// the /call allowlist refusal, and the liveness probe. A whitespace change is a
// failure here, which is the point.
func TestLoopbackHTTPResponsesAreByteForByteUnchanged(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	generic := (&httpServer{token: "tok", port: 7777}).handler(testCtx(t))

	const landing = "<!doctype html><meta charset=\"utf-8\"><title>mora loopback</title>\n" +
		"<script>window.__MORA_TOKEN=\"tok\";</script>\n" +
		"<body style=\"font:14px/1.5 system-ui;max-width:40rem;margin:3rem auto;color:#222\">\n" +
		"<h1>mora is serving locally</h1>\n" +
		"<p>This origin exposes your vault's memory tools over loopback HTTP for a connected\n" +
		"AI browser. The bearer token for this session is embedded on this page as\n" +
		"<code>window.__MORA_TOKEN</code> and readable only by same-origin script.</p>\n" +
		"<p>Endpoints: <code>GET /healthz</code>, <code>GET /brief</code>,\n" +
		"<code>POST /think</code>, <code>POST /search</code>, <code>GET /entity/{name}</code>,\n" +
		"<code>POST /meeting-prep</code>, <code>POST /write</code>, <code>POST /call</code>.</p>\n" +
		"</body>"

	for _, tc := range []struct {
		name, method, path, token, body string
		wantCode                        int
		wantBody                        string
		wantType                        string
	}{
		{
			name: "landing page", method: http.MethodGet, path: "/",
			wantCode: http.StatusOK, wantBody: landing, wantType: "text/html; charset=utf-8",
		},
		{
			name: "liveness probe", method: http.MethodGet, path: "/healthz",
			wantCode: http.StatusOK,
			wantBody: "{\n  \"ok\": true,\n  \"service\": \"mora\",\n  \"state\": \"healthy\",\n  \"version\": \"" + BuildVersion + "\"\n}\n",
			wantType: "application/json; charset=utf-8",
		},
		{
			name: "unauthenticated refusal", method: http.MethodGet, path: "/brief",
			wantCode: http.StatusUnauthorized,
			wantBody: "{\n  \"error\": \"unauthorized\"\n}\n",
			wantType: "application/json; charset=utf-8",
		},
		{
			name: "call allowlist refusal", method: http.MethodPost, path: "/call", token: "tok",
			body:     `{"name":"delete_memory","arguments":{"id":"x"}}`,
			wantCode: http.StatusForbidden,
			wantBody: "{\n  \"error\": \"tool not permitted over loopback HTTP\",\n  \"tool\": \"delete_memory\"\n}\n",
			wantType: "application/json; charset=utf-8",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Host = "127.0.0.1:7777"
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			generic.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("%s = %d, want %d", tc.name, rec.Code, tc.wantCode)
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Fatalf("%s body changed.\n got: %q\nwant: %q", tc.name, got, tc.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.wantType {
				t.Fatalf("%s content type = %q, want %q", tc.name, got, tc.wantType)
			}
		})
	}
}

// TestLoopbackHTTPRouteTableIsUnchanged pins the generic server's route set. A
// companion route accidentally mounted on the generic server would land here.
func TestLoopbackHTTPRouteTableIsUnchanged(t *testing.T) {
	got := (&httpServer{token: "tok", port: 7777}).httpRoutes(testCtx(t))
	want := []httpRoute{
		{Method: "GET", Pattern: "/healthz"},
		{Method: "GET", Pattern: "/{$}"},
		{Method: "POST", Pattern: "/call"},
		{Method: "POST", Pattern: "/think"},
		{Method: "POST", Pattern: "/search"},
		{Method: "POST", Pattern: "/write"},
		{Method: "POST", Pattern: "/meeting-prep"},
		{Method: "GET", Pattern: "/entity/{name}"},
		{Method: "GET", Pattern: "/brief"},
	}
	if len(got) != len(want) {
		t.Fatalf("the generic loopback API now has %d routes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("generic route %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// The projections carry the kernel's own freshness
// ---------------------------------------------------------------------------

// TestCompanionProjectionsCarryKernelFreshness drives all three routes against a
// real vault and asserts the honesty fields are the KERNEL's, not this file's.
func TestCompanionProjectionsCarryKernelFreshness(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "personal", "--type", "decision",
		"--title", "Ship the companion listener", "--text", "Three routes, device tokens only.")
	handler, token, cfg := companionTestListener(t)

	kernel := healthOf(cfg, cfg.OperationClock())
	wantState := string(companionHealthState(kernel.State))
	wantPolicy := configMCPWritePolicy(cfg)

	for _, tc := range []struct{ name, method, path, body string }{
		{"today", http.MethodGet, companion.RouteToday, ""},
		{"health", http.MethodGet, companion.RouteHealth, ""},
		{"context", http.MethodPost, companion.RouteContext, companionContextRequestBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, companionRequest(t, tc.method, tc.path, token, tc.body))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200\n%s", tc.name, rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"`+wantState+`"`) {
				t.Fatalf("%s does not carry the kernel's health state %q:\n%s", tc.name, wantState, body)
			}
			if !strings.Contains(body, `"`+wantPolicy+`"`) {
				t.Fatalf("%s does not carry the write policy %q:\n%s", tc.name, wantPolicy, body)
			}
			// Every projection declares its freshness collection even when it
			// is empty: an absent array is a claim nothing supports.
			key := "freshness"
			if tc.name == "health" {
				key = "sources"
			}
			if !strings.Contains(body, `"`+key+`"`) {
				t.Fatalf("%s carries no %s array:\n%s", tc.name, key, body)
			}
		})
	}
}

// TestCompanionContextRunsEveryMode proves all three modes of the frozen
// context_mode vocabulary answer, so no mode reaches a phone as a 503.
func TestCompanionContextRunsEveryMode(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "personal", "--type", "decision",
		"--title", "Launch date", "--text", "The launch is on the 14th, agreed with Sam.")
	handler, token, _ := companionTestListener(t)

	for _, mode := range []companion.ContextMode{companion.ModeThink, companion.ModeSearch, companion.ModeMeetingPrep} {
		t.Run(string(mode), func(t *testing.T) {
			body := fmt.Sprintf(`{"schema":"mora.companion.context.request","schema_version":1,"mode":%q,"query":"launch"}`, mode)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, companionRequest(t, http.MethodPost, companion.RouteContext, token, body))
			if rec.Code != http.StatusOK {
				t.Fatalf("mode %s = %d, want 200\n%s", mode, rec.Code, rec.Body.String())
			}
			var bundle companion.ContextBundle
			if err := companion.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
				t.Fatalf("mode %s produced a bundle the contract rejects: %v\n%s", mode, err, rec.Body.String())
			}
			if bundle.Mode != mode {
				t.Fatalf("mode %s came back as %s", mode, bundle.Mode)
			}
			if bundle.SynthesisPrompt == "" {
				t.Fatalf("mode %s carries no synthesis prompt; the shell has nothing to run", mode)
			}
		})
	}
}

// TestCompanionProjectionsCarryNoProviderIdentity is the leak gate for the
// derived identifiers. A Mora stable id is "<kind>/<provider id>" — shipping it
// would put the provider, the account and often the message id on the phone.
func TestCompanionProjectionsCarryNoProviderIdentity(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "personal", "--type", "decision",
		"--title", "Launch date", "--text", "The launch is on the 14th.")
	handler, token, cfg := companionTestListener(t)

	reader := newCompanionReader(cfg)
	bundle, err := reader.Context(testCtx(t), func() companion.ContextRequest {
		req := companion.NewContextRequest()
		req.Mode = companion.ModeSearch
		req.Query = "launch"
		return req
	}())
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if len(bundle.Evidence) == 0 {
		t.Fatal("the search bundle cited nothing; there is no derivation to check")
	}

	// The vault's own ids for the SAME memories, so the assertion is that the
	// projection does not carry them rather than that it happens to look tidy.
	mems, err := hybridSearch(testCtx(t), cfg, "launch", "", companionContextEvidence)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(mems) == 0 {
		t.Fatal("the vault returned nothing for the query the bundle answered")
	}
	shipped := strings.Join(func() []string {
		out := []string{}
		for _, row := range bundle.Evidence {
			out = append(out, row.MemoryID)
		}
		return out
	}(), " ")
	for _, m := range mems {
		if m.ID != "" && strings.Contains(shipped, m.ID) {
			t.Fatalf("the projection ships the vault's own id %q:\n%s", m.ID, shipped)
		}
	}
	for _, row := range bundle.Evidence {
		if !companionDerivedID.MatchString(row.MemoryID) {
			t.Fatalf("memory id %q is not the published mem_<32 hex> derivation", row.MemoryID)
		}
	}

	// And the derivation is stable: the same memory is the same id on the next
	// request, so a client can deduplicate across polls.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, companionRequest(t, http.MethodPost, companion.RouteContext, token, companionContextRequestBody))
	if !strings.Contains(rec.Body.String(), bundle.Evidence[0].MemoryID) {
		t.Fatalf("the derived memory id changed between two reads of the same memory")
	}
}

// TestCompanionOpaqueIDIsBoundedAndOpaque pins the derivation's two properties
// directly, including for an id far past the contract's identifier bound.
func TestCompanionOpaqueIDIsBoundedAndOpaque(t *testing.T) {
	for _, stable := range []string{
		"gmail_message/CAF=abc123@mail.gmail.com",
		"imessage/+15550100:" + strings.Repeat("x", 400),
		"",
		"a b c/ünicode",
	} {
		got := companionOpaqueID(companion.PrefixMemory, stable)
		if len(got) > companion.MaxIDBytes {
			t.Fatalf("companionOpaqueID(%q) is %d bytes, over the %d-byte bound", stable, len(got), companion.MaxIDBytes)
		}
		if !strings.HasPrefix(got, companion.PrefixMemory) {
			t.Fatalf("companionOpaqueID(%q) = %q, missing the prefix", stable, got)
		}
		if stable != "" && strings.Contains(got, stable) {
			t.Fatalf("companionOpaqueID(%q) = %q carries its input", stable, got)
		}
		if got != companionOpaqueID(companion.PrefixMemory, stable) {
			t.Fatalf("companionOpaqueID(%q) is not deterministic", stable)
		}
	}
}

// TestCompanionContextNeverBuildsAnIndex is the read-only guarantee.
//
// The kernel's read paths self-heal: hybridSearchTrace rebuilds when the index
// file is absent, ensureIndexDB rebuilds when the graph tables are missing, and
// openIndexRO rebuilds a schema-stale index. Each is minutes of disk and CPU
// over the whole vault, and each was reachable from one authenticated POST — a
// denial of service with a valid credential, which is the shape N22 exposes to
// a network.
//
// The test starts with NO index, drives every context mode, and asserts that no
// index file appeared and that the answer says so instead of pretending the
// vault had nothing to say.
func TestCompanionContextNeverBuildsAnIndex(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "personal", "--type", "decision",
		"--title", "Launch date", "--text", "The launch is on the 14th, agreed with Sam.")

	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	index := dbPath(cfg)
	if err := os.RemoveAll(index); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(index); !os.IsNotExist(err) {
		t.Fatalf("the index still exists after removal: %v", err)
	}
	handler, token, _ := companionTestListener(t)
	for _, mode := range []companion.ContextMode{companion.ModeThink, companion.ModeSearch, companion.ModeMeetingPrep} {
		t.Run(string(mode), func(t *testing.T) {
			body := fmt.Sprintf(`{"schema":"mora.companion.context.request","schema_version":1,"mode":%q,"query":"launch"}`, mode)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, companionRequest(t, http.MethodPost, companion.RouteContext, token, body))
			if rec.Code != http.StatusOK {
				t.Fatalf("mode %s = %d, want 200 — an unreadable index is reported in the body\n%s", mode, rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(index); !os.IsNotExist(err) {
				t.Fatalf("mode %s BUILT AN INDEX; a companion request must never write", mode)
			}

			var bundle companion.ContextBundle
			if uerr := companion.Unmarshal(rec.Body.Bytes(), &bundle); uerr != nil {
				t.Fatalf("mode %s produced a bundle the contract rejects: %v", mode, uerr)
			}
			if len(bundle.Evidence) != 0 {
				t.Fatalf("mode %s cited evidence with no index to retrieve it from", mode)
			}
			var said bool
			for _, gap := range bundle.Gaps {
				if strings.Contains(gap, "index") {
					said = true
				}
			}
			if !said {
				t.Fatalf("mode %s returned an empty bundle without saying the index is unreadable: %v", mode, bundle.Gaps)
			}
			// An empty answer must not read as a healthy one.
			if bundle.Health.State == companion.HealthHealthy {
				t.Fatalf("mode %s reported healthy with no index: %+v", mode, bundle.Health)
			}
		})
	}
}

// TestCompanionTodayNeverBuildsAnIndex is the same guarantee for the other
// expensive route. Today reads vault files rather than the index, so it answers
// — the assertion is that answering costs no rebuild.
func TestCompanionTodayNeverBuildsAnIndex(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "personal", "--type", "decision",
		"--title", "Ship it", "--text", "Three routes, device tokens only.")

	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	index := dbPath(cfg)
	if err := os.RemoveAll(index); err != nil {
		t.Fatal(err)
	}

	handler, token, _ := companionTestListener(t)
	for _, route := range []string{companion.RouteToday, companion.RouteHealth} {
		t.Run(route, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, companionRequest(t, http.MethodGet, route, token, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200\n%s", route, rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(index); !os.IsNotExist(err) {
				t.Fatalf("GET %s BUILT AN INDEX", route)
			}
			// An answer served without a usable index must not read as a
			// healthy one.
			if !strings.Contains(rec.Body.String(), `"unhealthy"`) {
				t.Fatalf("GET %s answered with no index and did not say so:\n%s", route, rec.Body.String())
			}
		})
	}
}

// TestReadOnlyRefusesEveryDurableRepair drives the read-only marker against the
// repair sites themselves.
//
// The previous shape asked a boundary predicate whether the index looked usable
// and called the kernel only when it did. That is a guess about which paths
// repair, and it was wrong twice — meeting_prep reached ensureIndexDB through
// the commitment inventory, think reached healShareIndex through a subscribed
// corpus — and it raced besides. This asserts the property at the site that
// decides it: with the marker on, a repair returns ErrReadOnlyRepairNeeded and
// writes nothing.
func TestReadOnlyRefusesEveryDurableRepair(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "personal", "--type", "fact", "--title", "A", "--text", "B")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	index := dbPath(cfg)
	readOnly := withReadOnly(testCtx(t))

	t.Run("rebuild refuses and writes nothing", func(t *testing.T) {
		if rerr := os.Remove(index); rerr != nil {
			t.Fatal(rerr)
		}
		if _, rerr := rebuildIndex(readOnly, cfg); !errors.Is(rerr, ErrReadOnlyRepairNeeded) {
			t.Fatalf("rebuildIndex under the marker = %v, want ErrReadOnlyRepairNeeded", rerr)
		}
		if _, serr := os.Stat(index); !os.IsNotExist(serr) {
			t.Fatal("a refused rebuild still created the index")
		}
	})

	t.Run("hybridSearch refuses rather than rebuilding", func(t *testing.T) {
		if _, serr := os.Stat(index); !os.IsNotExist(serr) {
			t.Fatal("the fixture expects an absent index")
		}
		if _, serr := hybridSearch(readOnly, cfg, "anything", "", 4); !errors.Is(serr, ErrReadOnlyRepairNeeded) {
			t.Fatalf("hybridSearch under the marker = %v, want ErrReadOnlyRepairNeeded", serr)
		}
		if _, serr := os.Stat(index); !os.IsNotExist(serr) {
			t.Fatal("a refused search still built the index")
		}
	})

	t.Run("the commitment inventory refuses rather than rebuilding", func(t *testing.T) {
		// This is the path meeting_prep and the brief both reach, and the one
		// the round-2 review found still writing.
		if _, ierr := readCommitmentInventory(readOnly, cfg, cfg.OperationClock()); !errors.Is(ierr, ErrReadOnlyRepairNeeded) {
			t.Fatalf("readCommitmentInventory under the marker = %v, want ErrReadOnlyRepairNeeded", ierr)
		}
		if _, serr := os.Stat(index); !os.IsNotExist(serr) {
			t.Fatal("a refused inventory read still built the index")
		}
	})

	t.Run("healShareIndex refuses rather than publishing a repair", func(t *testing.T) {
		if herr := healShareIndex(readOnly, cfg, "any-share"); !errors.Is(herr, ErrReadOnlyRepairNeeded) {
			t.Fatalf("healShareIndex under the marker = %v, want ErrReadOnlyRepairNeeded", herr)
		}
	})

	t.Run("a schema-stale index is refused, not healed", func(t *testing.T) {
		// openIndexRO's auto-heal is a rebuild, so it lands on the same guard.
		// The fixture is a valid database whose user_version this binary does
		// not understand.
		if _, rerr := rebuildIndex(testCtx(t), cfg); rerr != nil {
			t.Fatal(rerr)
		}
		db, derr := sql.Open("sqlite", rwIndexDSN(cfg))
		if derr != nil {
			t.Fatal(derr)
		}
		if _, derr := db.Exec(`PRAGMA user_version = 999`); derr != nil {
			t.Fatal(derr)
		}
		if cerr := db.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		before, serr := os.Stat(index)
		if serr != nil {
			t.Fatal(serr)
		}
		if _, oerr := openIndexRO(readOnly, cfg); oerr == nil {
			t.Fatal("a schema-stale index opened cleanly under the marker")
		}
		after, serr := os.Stat(index)
		if serr != nil {
			t.Fatal(serr)
		}
		if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
			t.Fatal("the schema-stale index was rewritten under the read-only marker")
		}
	})
}

// TestCompanionMeetingPrepWithARealEventBuildsNoIndex is the round-2 finding,
// reproduced and closed.
//
// The earlier no-index test seeded no calendar event, so buildNextMeetingBrief
// returned its empty brief before it ever reached the commitment inventory —
// the test passed while the write path it claimed to cover was never entered.
// This one seeds a real event with real attendees, so the brief goes all the way
// to readCommitmentInventory, which is where ensureIndexDB used to rebuild.
func TestCompanionMeetingPrepWithARealEventBuildsNoIndex(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	pinPrepClock(t, now)
	// The self address has to be resolvable or the brief gaps out before it
	// reaches the commitment inventory — which is exactly how the test this
	// replaces managed to pass without covering anything.
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@a.com",
		Enabled: genericutil.Ptr(true), CreatedAt: now.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Acme sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya", "me@a.com": "Me"}, "me@a.com", "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	// Build the index once so the event is real and resolvable, then take it
	// away: this is the state a phone can find the Mac in.
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	index := dbPath(cfg)
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}

	// The unmarked path still reaches the event, which proves the fixture is
	// live rather than exiting early like the one it replaces.
	brief, err := buildNextMeetingBrief(testCtx(t), cfg, now, nil, 0, meetingBriefDefaultPerGuest)
	if err != nil {
		t.Fatalf("the fixture does not produce a brief at all: %v", err)
	}
	if brief.Event == nil {
		t.Fatal("the fixture seeded no reachable event, so it cannot exercise the commitment inventory")
	}
	if _, serr := os.Stat(index); serr != nil {
		t.Fatal("the unmarked brief did not rebuild; the fixture no longer covers the write path")
	}
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}

	handler, token, _ := companionTestListener(t)
	body := `{"schema":"mora.companion.context.request","schema_version":1,"mode":"meeting_prep","query":"Riya"}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, companionRequest(t, http.MethodPost, companion.RouteContext, token, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("meeting_prep = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if _, serr := os.Stat(index); !os.IsNotExist(serr) {
		t.Fatal("meeting_prep with a real event BUILT AN INDEX")
	}
	var bundle companion.ContextBundle
	if uerr := companion.Unmarshal(rec.Body.Bytes(), &bundle); uerr != nil {
		t.Fatalf("meeting_prep produced a bundle the contract rejects: %v", uerr)
	}
	var said bool
	for _, gap := range bundle.Gaps {
		if strings.Contains(gap, "index") {
			said = true
		}
	}
	if !said {
		t.Fatalf("meeting_prep returned a bundle without saying the index is unreadable: %v", bundle.Gaps)
	}
}

// TestCompanionThinkPublishesNoShareRepair is the other round-2 finding.
//
// A corrupt subscribed-share index makes search re-cut and PUBLISH a repair
// generation from the head's frozen corpus. That is the right answer for a human
// running `mora search`; it is a durable write triggered by an authenticated
// HTTP request, which is not.
func TestCompanionThinkPublishesNoShareRepair(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil sqlite decision", "neil standardized on sqlite too"),
	})
	commit, ok, err := resolvePublishedCommit(cfg, "neil")
	if err != nil || !ok {
		t.Fatalf("no published generation to corrupt: %v", err)
	}
	if err := os.WriteFile(shareGenIndexPath(cfg, "neil", commit.Gen), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}

	handler, token, _ := companionTestListener(t)
	body := `{"schema":"mora.companion.context.request","schema_version":1,"mode":"think","query":"sqlite"}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, companionRequest(t, http.MethodPost, companion.RouteContext, token, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("think = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	after, ok, err := resolvePublishedCommit(cfg, "neil")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the subscription lost its published generation")
	}
	if after.Gen != commit.Gen {
		t.Fatalf("think PUBLISHED a repair generation: %s -> %s", commit.Gen, after.Gen)
	}

	// Not publishing is half the property. The other half is SAYING so: a
	// subscription dropped from the corpus without a word is a confident answer
	// built from part of the evidence, which is the failure the whole gap
	// discipline exists to prevent.
	var bundle companion.ContextBundle
	if uerr := companion.Unmarshal(rec.Body.Bytes(), &bundle); uerr != nil {
		t.Fatalf("think produced a bundle the contract rejects: %v\n%s", uerr, rec.Body.String())
	}
	var said bool
	for _, gap := range bundle.Gaps {
		if strings.Contains(gap, "index") {
			said = true
		}
	}
	if !said {
		t.Fatalf("think dropped a corrupt subscription silently; gaps = %v", bundle.Gaps)
	}
	if bundle.Health.State == companion.HealthHealthy {
		t.Fatalf("think reported healthy while a subscription was unreadable: %+v", bundle.Health)
	}

	// And the unmarked path still heals, so the refusal is the marker's doing
	// and not a broken heal.
	if _, _, serr := searchSharedCorporaProbe(t, cfg); serr != nil {
		t.Fatalf("the unmarked search failed: %v", serr)
	}
	healed, ok, err := resolvePublishedCommit(cfg, "neil")
	if err != nil || !ok {
		t.Fatalf("resolve after the unmarked search: %v", err)
	}
	if healed.Gen == commit.Gen {
		t.Fatal("the unmarked search did not heal; the fixture no longer covers the write path")
	}
}

// TestSearchSharedCorporaSurfacesTheReadOnlyRefusal pins the propagation itself,
// at the function that used to swallow it.
//
// Every OTHER heal failure stays suppressed — a share that genuinely cannot be
// re-cut is excluded from this search and surfaced by doctor, and taking local
// recall down over it would be the wrong trade. The read-only refusal is the one
// that must travel, because it means the share COULD have been served and the
// caller asked for no work.
func TestSearchSharedCorporaSurfacesTheReadOnlyRefusal(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil sqlite decision", "neil standardized on sqlite too"),
	})
	commit, ok, err := resolvePublishedCommit(cfg, "neil")
	if err != nil || !ok {
		t.Fatalf("no published generation to corrupt: %v", err)
	}
	if err := os.WriteFile(shareGenIndexPath(cfg, "neil", commit.Gen), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Under the marker: the refusal reaches the caller, and nothing was
	// published.
	if _, serr := searchSharedCorpora(withReadOnly(testCtx(t)), cfg, "sqlite", "", 8); !errors.Is(serr, ErrReadOnlyRepairNeeded) {
		t.Fatalf("searchSharedCorpora under the marker = %v, want ErrReadOnlyRepairNeeded", serr)
	}
	refusedGen, ok, err := resolvePublishedCommit(cfg, "neil")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if refusedGen.Gen != commit.Gen {
		t.Fatal("the refused search published a repair generation anyway")
	}

	// Without it: unchanged. The share heals, the search succeeds, and no error
	// reaches the caller — which is what every non-companion surface depends on.
	if _, serr := searchSharedCorpora(testCtx(t), cfg, "sqlite", "", 8); serr != nil {
		t.Fatalf("the unmarked search returned %v, want nil", serr)
	}
	healed, ok, err := resolvePublishedCommit(cfg, "neil")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if healed.Gen == commit.Gen {
		t.Fatal("the unmarked search did not heal; the fixture no longer covers the write path")
	}

	// And a heal that genuinely CANNOT run stays suppressed. This is the other
	// half of the rule and the half a blanket propagation would break: a share
	// whose frozen corpus is itself unreadable can never be re-cut, doctor is
	// where an operator hears about it, and taking local recall down over one
	// unrepairable subscription would be the wrong trade.
	if err := os.WriteFile(shareGenIndexPath(cfg, "neil", healed.Gen), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(shareGenCorpusDir(cfg, "neil", healed.Gen)); err != nil {
		t.Fatal(err)
	}
	if herr := healShareIndex(testCtx(t), cfg, "neil"); herr == nil {
		t.Fatal("the fixture still heals, so it cannot exercise the suppressed path")
	} else if errors.Is(herr, ErrReadOnlyRepairNeeded) {
		t.Fatalf("the fixture produced the read-only refusal, not an ordinary heal failure: %v", herr)
	}
	if _, serr := searchSharedCorpora(testCtx(t), cfg, "sqlite", "", 8); serr != nil {
		t.Fatalf("an unrepairable share took the whole search down: %v — only the read-only refusal may travel", serr)
	}
}

// searchSharedCorporaProbe runs the shared-corpus arm with an ordinary context,
// which is the path that heals.
func searchSharedCorporaProbe(t *testing.T, cfg Config) ([][]Memory, bool, error) {
	t.Helper()
	res, err := searchSharedCorpora(testCtx(t), cfg, "sqlite", "", 8)
	return res, true, err
}

// TestReadOnlyIsOffForEveryOtherCaller is the regression guard the ruling asked
// for. The marker must change nothing for the CLI, MCP, or the generic loopback
// API: those callers still get the self-healing kernel they have always had.
func TestReadOnlyIsOffForEveryOtherCaller(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "personal", "--type", "fact", "--title", "A", "--text", "B")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	index := dbPath(cfg)

	if readOnlyCall(testCtx(t)) {
		t.Fatal("an ordinary context reads as read-only")
	}
	if readOnlyCall(nil) { //nolint:staticcheck // a nil context is exactly the case under test
		t.Fatal("a nil context reads as read-only; the guard must fail open for callers that pass one")
	}

	if rerr := os.Remove(index); rerr != nil {
		t.Fatal(rerr)
	}
	// An unmarked search rebuilds, exactly as it did before this node.
	if _, serr := hybridSearch(testCtx(t), cfg, "A", "", 4); serr != nil {
		t.Fatalf("an unmarked search failed: %v", serr)
	}
	if _, serr := os.Stat(index); serr != nil {
		t.Fatalf("an unmarked search did not rebuild the index: %v", serr)
	}

	if rerr := os.Remove(index); rerr != nil {
		t.Fatal(rerr)
	}
	if _, ierr := readCommitmentInventory(testCtx(t), cfg, cfg.OperationClock()); ierr != nil {
		t.Fatalf("an unmarked inventory read failed: %v", ierr)
	}
	if _, serr := os.Stat(index); serr != nil {
		t.Fatalf("an unmarked inventory read did not rebuild the index: %v", serr)
	}
}

// TestCompanionTodayIsCachedUntilTheIndexMoves pins the work budget's other
// half. The concurrency limit bounds simultaneous work; the cache bounds
// REPEATED work, which is what a polling phone actually generates.
func TestCompanionTodayIsCachedUntilTheIndexMoves(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "personal", "--type", "decision",
		"--title", "First", "--text", "The first decision.")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	reader := newCompanionReader(cfg)

	first, err := reader.Today(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if !reader.valid {
		t.Fatal("the first Today did not populate the cache")
	}

	// A second read at the same index state is served from the cache. Proving
	// that without timing: poison the cached value and require it back.
	reader.mu.Lock()
	poisoned := reader.today
	poisoned.Truncated = !poisoned.Truncated
	reader.today = poisoned
	reader.mu.Unlock()

	second, err := reader.Today(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if second.Truncated != poisoned.Truncated {
		t.Fatal("the second Today re-walked the vault instead of using the cache")
	}

	// A changed index state retires the entry, even inside the TTL: the answer
	// is not merely old, it is wrong.
	reader.mu.Lock()
	reader.key = "a different index state"
	reader.mu.Unlock()
	third, err := reader.Today(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if third.Truncated != first.Truncated {
		t.Fatal("a changed index state did not retire the cached Today")
	}

	// And the TTL is the floor under everything the key cannot see.
	reader.mu.Lock()
	reader.cached = reader.cached.Add(-2 * companionTodayTTL)
	reader.today = poisoned
	reader.mu.Unlock()
	fourth, err := reader.Today(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Truncated != first.Truncated {
		t.Fatal("an expired entry was served")
	}
}

// TestCompanionIndexStateFollowsTheContractTable pins the collapse N02
// published in docs/companion-contract.md.
//
// It is written as the full table rather than as a spot check because the
// interesting row is `dirty`. The kernel's own aggregate folds dirty into
// unhealthy — that is `mora doctor`'s fail-closed verdict — and reusing that
// collapse here would tell a phone that a behind-but-usable index and a missing
// one are the same thing.
func TestCompanionIndexStateFollowsTheContractTable(t *testing.T) {
	for _, tc := range []struct {
		kernel string
		want   companion.HealthState
	}{
		{healthpkg.IndexFresh, companion.HealthHealthy},
		{healthpkg.IndexDirty, companion.HealthDegraded},
		{healthpkg.IndexDegraded, companion.HealthDegraded},
		{healthpkg.IndexFailed, companion.HealthUnhealthy},
		{healthpkg.IndexNever, companion.HealthUnhealthy},
		{"", companion.HealthUnhealthy},
		{"something the kernel grew later", companion.HealthUnhealthy},
	} {
		t.Run(tc.kernel, func(t *testing.T) {
			if got := companionIndexState(tc.kernel); got != tc.want {
				t.Fatalf("index state %q maps to %q, want %q", tc.kernel, got, tc.want)
			}
		})
	}
}

// TestCompanionHealthStateIsTheKernelAggregate pins the other collapse: the
// top-level state is the kernel's fail-closed verdict, passed through.
func TestCompanionHealthStateIsTheKernelAggregate(t *testing.T) {
	for _, tc := range []struct {
		kernel string
		want   companion.HealthState
	}{
		{healthHealthy, companion.HealthHealthy},
		{healthDegraded, companion.HealthDegraded},
		{healthUnhealthy, companion.HealthUnhealthy},
		{"", companion.HealthUnhealthy},
		{"unrecognized", companion.HealthUnhealthy},
	} {
		t.Run(tc.kernel, func(t *testing.T) {
			if got := companionHealthState(tc.kernel); got != tc.want {
				t.Fatalf("health state %q maps to %q, want %q", tc.kernel, got, tc.want)
			}
		})
	}
}

// TestCompanionFreshnessCoversEveryCombination is the fidelity table, and it is
// a real Cartesian product this time.
//
// Four kernel states crossed with present/absent last_success_at crossed with
// present/absent error_code is sixteen rows, and the previous version claimed
// the table while covering nine of them. Every output field is asserted, because
// the failures this exists to catch are field-shaped: a dropped error_code, an
// age that survived a state change, a timestamp on a row that claims never.
//
// The row that used to be wrong is failed/absent/present — an unreadable
// sources.json — which collapsed to "never" and lost its code, so a phone showed
// a source that had simply never run rather than one that is broken now.
func TestCompanionFreshnessCoversEveryCombination(t *testing.T) {
	const generated = "2026-09-04T12:00:00Z"
	const last = "2026-09-04T11:00:00Z"
	const hour = int64(3600)

	states := []struct {
		name   string
		kernel string
	}{
		{"fresh", healthpkg.Fresh},
		{"stale", healthpkg.Stale},
		{"failed", healthpkg.Failed},
		{"never", healthpkg.Never},
	}
	stamps := []struct {
		name  string
		value string
	}{
		{"with a last success", last},
		{"with no last success", ""},
	}
	codes := []struct {
		name  string
		value string
	}{
		{"with a typed code", errCodeConnectorUnauthorized},
		{"with no code", ""},
	}

	// want is the whole published output for each of the sixteen inputs.
	want := map[string]companion.SourceFreshness{
		"fresh/with a last success/with a typed code":   {State: companion.FreshnessFresh, AgeSeconds: hour, LastSuccessAt: last},
		"fresh/with a last success/with no code":        {State: companion.FreshnessFresh, AgeSeconds: hour, LastSuccessAt: last},
		"fresh/with no last success/with a typed code":  {State: companion.FreshnessNever, AgeSeconds: -1},
		"fresh/with no last success/with no code":       {State: companion.FreshnessNever, AgeSeconds: -1},
		"stale/with a last success/with a typed code":   {State: companion.FreshnessStale, AgeSeconds: hour, LastSuccessAt: last, ErrorCode: companion.ErrAuthExpired},
		"stale/with a last success/with no code":        {State: companion.FreshnessStale, AgeSeconds: hour, LastSuccessAt: last},
		"stale/with no last success/with a typed code":  {State: companion.FreshnessNever, AgeSeconds: -1},
		"stale/with no last success/with no code":       {State: companion.FreshnessNever, AgeSeconds: -1},
		"failed/with a last success/with a typed code":  {State: companion.FreshnessFailed, AgeSeconds: hour, LastSuccessAt: last, ErrorCode: companion.ErrAuthExpired},
		"failed/with a last success/with no code":       {State: companion.FreshnessFailed, AgeSeconds: hour, LastSuccessAt: last, ErrorCode: companion.ErrInternal},
		"failed/with no last success/with a typed code": {State: companion.FreshnessFailed, AgeSeconds: -1, ErrorCode: companion.ErrAuthExpired},
		"failed/with no last success/with no code":      {State: companion.FreshnessFailed, AgeSeconds: -1, ErrorCode: companion.ErrInternal},
		"never/with a last success/with a typed code":   {State: companion.FreshnessNever, AgeSeconds: -1},
		"never/with a last success/with no code":        {State: companion.FreshnessNever, AgeSeconds: -1},
		"never/with no last success/with a typed code":  {State: companion.FreshnessNever, AgeSeconds: -1},
		"never/with no last success/with no code":       {State: companion.FreshnessNever, AgeSeconds: -1},
	}
	if len(want) != len(states)*len(stamps)*len(codes) {
		t.Fatalf("the expectation table has %d rows, want %d — it is not the full product",
			len(want), len(states)*len(stamps)*len(codes))
	}

	covered := map[string]bool{}
	for _, st := range states {
		for _, ts := range stamps {
			for _, code := range codes {
				name := st.name + "/" + ts.name + "/" + code.name
				t.Run(name, func(t *testing.T) {
					expected, ok := want[name]
					if !ok {
						t.Fatalf("no expectation for %q", name)
					}
					expected.Key = "gmail:work"
					covered[name] = true

					rows := companionFreshness([]healthpkg.Source{{
						Key:           "gmail:work",
						State:         st.kernel,
						LastSuccessAt: ts.value,
						ErrorCode:     code.value,
					}}, generated)
					if len(rows) != 1 {
						t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
					}
					got := rows[0]

					// Every field, named, so a regression says which one moved.
					if got.Key != expected.Key {
						t.Fatalf("key = %q, want %q", got.Key, expected.Key)
					}
					if got.State != expected.State {
						t.Fatalf("state = %q, want %q", got.State, expected.State)
					}
					if got.AgeSeconds != expected.AgeSeconds {
						t.Fatalf("age_seconds = %d, want %d", got.AgeSeconds, expected.AgeSeconds)
					}
					if got.LastSuccessAt != expected.LastSuccessAt {
						t.Fatalf("last_success_at = %q, want %q", got.LastSuccessAt, expected.LastSuccessAt)
					}
					if got.ErrorCode != expected.ErrorCode {
						t.Fatalf("error_code = %q, want %q", got.ErrorCode, expected.ErrorCode)
					}

					// And the contract must accept it, or the fidelity is
					// theoretical.
					projection := companion.NewHealthProjection()
					projection.GeneratedAt = generated
					projection.State = companion.HealthUnhealthy
					projection.Policy = companion.PolicyReadonly
					projection.Index = companion.IndexHealth{State: companion.HealthUnhealthy}
					projection.Sources = rows
					if err := projection.Validate(); err != nil {
						t.Fatalf("the translated row does not satisfy the contract: %v", err)
					}
				})
			}
		}
	}
	if len(covered) != len(want) {
		t.Fatalf("drove %d of %d combinations", len(covered), len(want))
	}
}

// TestCompanionUnreadableSourcesConfigReachesTheWire is the end-to-end form of
// the row above: the kernel's own fail-closed answer for a corrupt sources.json
// has to survive the translation and appear on the health route.
func TestCompanionUnreadableSourcesConfigReachesTheWire(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}

	kernel := sourceHealthAll(cfg, cfg.OperationClock())
	rows := companionFreshness(kernel, companionStamp(cfg.OperationClock()))
	for i, src := range kernel {
		if src.State != healthpkg.Failed {
			continue
		}
		if rows[i].State != companion.FreshnessFailed {
			t.Fatalf("kernel source %q is failed but reached the wire as %q", src.Key, rows[i].State)
		}
		if rows[i].ErrorCode == "" {
			t.Fatalf("kernel source %q reached the wire as a failure with no reason", src.Key)
		}
	}
}

// TestCompanionFreshnessAgeIsExact pins the age arithmetic the contract checks
// with no tolerance, including the two shapes that would otherwise ship a lie:
// a source that never succeeded, and a stored success later than the
// projection's own clock.
func TestCompanionFreshnessAgeIsExact(t *testing.T) {
	const generated = "2026-09-04T12:00:00Z"
	rows := companionFreshness([]healthpkg.Source{
		{Key: "gmail:work", State: healthpkg.Fresh, LastSuccessAt: "2026-09-04T11:45:30Z"},
		{Key: "imessage", State: healthpkg.Stale, LastSuccessAt: "2026-09-01T12:00:00Z"},
		{Key: "github", State: healthpkg.Failed, LastSuccessAt: "2026-09-04T10:00:00Z", ErrorCode: errCodeConnectorUnauthorized},
		{Key: "applecalendar", State: healthpkg.Never},
		{Key: "skewed", State: healthpkg.Fresh, LastSuccessAt: "2026-09-04T12:00:10Z"},
		{Key: "unparseable", State: healthpkg.Fresh, LastSuccessAt: "last tuesday"},
	}, generated)

	want := []companion.SourceFreshness{
		{Key: "gmail:work", State: companion.FreshnessFresh, AgeSeconds: 870, LastSuccessAt: "2026-09-04T11:45:30Z"},
		{Key: "imessage", State: companion.FreshnessStale, AgeSeconds: 259200, LastSuccessAt: "2026-09-01T12:00:00Z"},
		{Key: "github", State: companion.FreshnessFailed, AgeSeconds: 7200, LastSuccessAt: "2026-09-04T10:00:00Z", ErrorCode: companion.ErrAuthExpired},
		{Key: "applecalendar", State: companion.FreshnessNever, AgeSeconds: -1},
		{Key: "skewed", State: companion.FreshnessFresh, AgeSeconds: 0, LastSuccessAt: generated},
		{Key: "unparseable", State: companion.FreshnessNever, AgeSeconds: -1},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d freshness rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}

	// And the contract itself agrees, which is the check that matters: the age
	// has to be EXACTLY the distance between the two timestamps beside it.
	projection := companion.NewHealthProjection()
	projection.GeneratedAt = generated
	projection.State = companion.HealthDegraded
	projection.Policy = companion.PolicyPropose
	projection.Index = companion.IndexHealth{State: companion.HealthHealthy}
	projection.Sources = rows
	if err := projection.Validate(); err != nil {
		t.Fatalf("the translated freshness rows do not satisfy the contract: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The subcommand
// ---------------------------------------------------------------------------

func TestCompanionServeRefusesJSONAndBadArguments(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"json", []string{"companion", "serve", "--json"}, "does not support --json"},
		{"zero port", []string{"companion", "serve", "--port", "0"}, "must be between"},
		{"high port", []string{"companion", "serve", "--port", "70000"}, "must be between"},
		{"stray positional", []string{"companion", "serve", "extra"}, "unexpected argument"},
		{"unknown flag", []string{"companion", "serve", "--addr", "0.0.0.0:1"}, "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := Run(testCtx(t), tc.args, &out, &out, strings.NewReader(""))
			if err == nil {
				t.Fatalf("mora %s: want an error, got none\n%s", strings.Join(tc.args, " "), out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mora %s: error %q does not mention %q", strings.Join(tc.args, " "), err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("a refused server start wrote to stdout:\n%s", out.String())
			}
		})
	}
}

// TestCompanionServeBindsAndAnswersOneRequest is the end-to-end witness for the
// subcommand: the real Run path, a real listener on a real socket, a real
// device token, and a real projection back.
//
// The port is taken by binding :0 and releasing it, because `--port 0` is a
// refusal (a server on an ephemeral port is a server nobody can find). That
// leaves a small window for another process to claim it, so a bind failure
// skips rather than fails — a port race is not a defect in this code.
func TestCompanionServeBindsAndAnswersOneRequest(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	probe, err := net.Listen("tcp", companion.LoopbackHost+":0")
	if err != nil {
		t.Skipf("no loopback port available: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	reg := companion.NewRegistry(cfg.ConfigDir, cfg.StateDir,
		companion.WithClock(func() time.Time { return cfg.OperationClock() }))
	payload, err := reg.Pair("phone", companion.PlatformIOS,
		fmt.Sprintf("http://%s:%d", companion.LoopbackHost, port))
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	var banner lockedWriter
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"companion", "serve", "--port", strconv.Itoa(port)}, &banner, &banner, strings.NewReader(""))
	}()

	base := fmt.Sprintf("http://%s:%d", companion.LoopbackHost, port)
	client := &http.Client{Timeout: 5 * time.Second}
	var resp *http.Response
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, base+companion.RouteHealth, nil)
		if rerr != nil {
			t.Fatal(rerr)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = client.Do(req)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		<-done
		t.Skipf("the listener never came up on the probed port (%v); banner:\n%s", err, banner.String())
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d\n%s", companion.RouteHealth, resp.StatusCode, body)
	}
	var projection companion.HealthProjection
	if uerr := companion.Unmarshal(body, &projection); uerr != nil {
		t.Fatalf("the live listener served a projection the contract rejects: %v\n%s", uerr, body)
	}

	// The same socket refuses an unpaired caller.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+companion.RouteHealth, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer not-a-device-token")
	refused, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	refused.Body.Close()
	if refused.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unpaired caller got %d on the live listener, want 401", refused.StatusCode)
	}

	cancel()
	if serveErr := <-done; serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		t.Fatalf("companion serve: %v", serveErr)
	}
	if strings.Contains(banner.String(), token) {
		t.Fatal("the startup banner printed a device token")
	}
}

// lockedWriter lets the test read the banner while Run writes it from another
// goroutine.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestCompanionAppearsInTheHelp closes the N11 carry-over: a command the help
// does not name is a command nobody finds.
func TestCompanionAppearsInTheHelp(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out)
	usage := out.String()
	for _, want := range []string{
		"mora companion pair",
		"mora companion list",
		"mora companion revoke",
		"mora companion serve",
	} {
		if !strings.Contains(usage, want) {
			t.Fatalf("printUsage does not mention %q:\n%s", want, usage)
		}
	}
}

// TestCompanionUsageNamesServe pins the group's own usage line, which is what a
// mistyped subcommand prints.
func TestCompanionUsageNamesServe(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var out bytes.Buffer
	err := Run(testCtx(t), []string{"companion", "srve"}, &out, &out, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "serve") {
		t.Fatalf("an unknown companion subcommand does not name serve: %v", err)
	}
}

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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/companion"
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
	srv, err := companion.NewServer(companion.ServerOptions{
		Addr:    fmt.Sprintf("%s:%d", companion.LoopbackHost, defaultCompanionPort),
		Devices: reg,
		Reader:  companionReader{cfg: cfg},
		Now:     cfg.OperationClock,
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

	reader := companionReader{cfg: cfg}
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

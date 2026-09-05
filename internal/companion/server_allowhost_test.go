package companion

// The N22 half of the host guard.
//
// The listener binds 127.0.0.1 and nothing else, before and after this file.
// What changes is which Host header it will answer, and the reason it has to
// change is measurable: a reverse proxy in front of a loopback backend forwards
// the CLIENT's Host verbatim. Measured against tailscale 1.102.3 on macOS, a
// request to the published node name arrives with Host set to that name and
// RemoteAddr set to 127.0.0.1, so the pre-N22 guard answered 403 forbidden_host
// to every request a paired phone made.
//
// Every hostname here is a documentation placeholder under the RFC 2606 .example
// TLD. A real tailnet name is a fact about someone's network.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testPublishedHost = "desk-node.tailnet-example.example"

// allowHostServer is testServer with the opt-in host set.
func allowHostServer(t *testing.T, allowHost string) (*Server, string, *bytes.Buffer) {
	t.Helper()
	reg, _, _, _ := testRegistry(t)
	token, _ := pairAndConfirm(t, reg, "phone")
	log := &bytes.Buffer{}
	srv, err := NewServer(ServerOptions{
		Addr:      "127.0.0.1:7778",
		AllowHost: allowHost,
		Devices:   reg,
		Pairings:  reg,
		Reader:    newStubReader(),
		Writer:    newStubWriter(),
		Captures:  NewReservationStore(t.TempDir()),
		Now:       func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
		Log:       log,
	})
	if err != nil {
		t.Fatalf("NewServer(AllowHost=%q): %v", allowHost, err)
	}
	return srv, token, log
}

// proxied builds the request Tailscale Serve actually makes: the client's Host,
// the proxy's loopback peer address, and the forwarding headers tailscaled adds.
func proxied(host, token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, RouteHealth, nil)
	r.Host = host
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	r.Header.Set("X-Forwarded-Host", host)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAllowHostIsOffByDefaultAndTheGuardIsUnchanged(t *testing.T) {
	srv, _, _, token, _ := testServer(t)

	// A listener built with no AllowHost refuses the published name exactly as
	// it refused every other non-loopback Host before this field existed.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, proxied(testPublishedHost, token))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 with no AllowHost set", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "forbidden_host") {
		t.Fatalf("body = %q, want forbidden_host", rec.Body.String())
	}
}

func TestAllowHostAdmitsExactlyOneNameFromALoopbackPeer(t *testing.T) {
	srv, token, _ := allowHostServer(t, testPublishedHost)
	handler := srv.Handler()

	// The named host, proxied from loopback, is served.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, proxied(testPublishedHost, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q, want 200 for the published host", rec.Code, rec.Body.String())
	}

	// Host names are case-insensitive on the wire, so folding case is not a
	// widening. Everything else would be.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, proxied(strings.ToUpper(testPublishedHost), token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the published host in a different case", rec.Code)
	}

	// The loopback arm still works, which is what makes AllowHost additive: a
	// client on the Mac does not have to know the published name.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodGet, RouteHealth, token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a direct loopback request", rec.Code)
	}
}

func TestAllowHostRefusesEverythingItDoesNotNameExactly(t *testing.T) {
	srv, token, _ := allowHostServer(t, testPublishedHost)
	handler := srv.Handler()

	for _, tc := range []struct {
		name string
		host string
	}{
		{"a different node on the same tailnet", "other." + testPublishedHost},
		{"a parent suffix", "tailnet-example.example"},
		{"the same name with a port the mapping does not use", testPublishedHost + ":8443"},
		{"a name that merely contains it", "evil-" + testPublishedHost},
		{"a name it is a prefix of", strings.TrimSuffix(testPublishedHost, ".example")},
		{"an attacker's own name", "attacker.example"},
		// strings.EqualFold would ACCEPT these: Unicode simple folding maps
		// U+212A KELVIN SIGN onto "k" and U+017F LATIN SMALL LETTER LONG S onto
		// "s", so a Host that is not this name byte-wise would compare equal to
		// it. The comparison folds ASCII case only, which is why they are 403.
		{"a Kelvin sign standing in for k", strings.Replace(testPublishedHost, "k", "\u212a", 1)},
		{"a long s standing in for s", strings.Replace(testPublishedHost, "s", "\u017f", 1)},
		{"a trailing dot", testPublishedHost + "."},
		{"an empty host", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, proxied(tc.host, token))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("Host %q got %d, want 403", tc.host, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "forbidden_host") {
				t.Fatalf("Host %q body = %q, want forbidden_host", tc.host, rec.Body.String())
			}
		})
	}
}

func TestAllowHostStillRequiresALoopbackPeer(t *testing.T) {
	srv, token, _ := allowHostServer(t, testPublishedHost)
	handler := srv.Handler()

	// Serve dials the backend from 127.0.0.1. A request carrying the published
	// name from any other peer did not come through the proxy, and the name
	// alone must not be enough — this is the arm that keeps the boundary honest
	// if the port is ever bound wider than it is today.
	for _, tc := range []struct {
		name       string
		remoteAddr string
	}{
		{"a LAN peer", "192.0.2.10:41000"},
		{"another tailnet node", "198.51.100.7:41000"},
		{"an unparseable peer", "not-an-address"},
		{"no peer at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := proxied(testPublishedHost, token)
			r.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("peer %q got %d, want 403", tc.remoteAddr, rec.Code)
			}
		})
	}

	// IPv6 loopback is a loopback peer. The BIND is still the literal 127.0.0.1
	// — this is about who dialled it, not what was bound.
	r := proxied(testPublishedHost, token)
	r.RemoteAddr = "[::1]:41000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("an ::1 peer got %d, want 200", rec.Code)
	}
}

func TestAllowHostDoesNotBecomeACredential(t *testing.T) {
	srv, _, _ := allowHostServer(t, testPublishedHost)
	handler := srv.Handler()

	// Past the host guard, the device token is still the only thing that
	// authenticates. The Tailscale-User-* identity headers tailscaled injects
	// are a real signed assertion about a tailnet user and are still worth
	// nothing here: this listener answers to a paired DEVICE.
	r := proxied(testPublishedHost, "")
	r.Header.Set("Tailscale-User-Login", "someone@example.com")
	r.Header.Set("Tailscale-User-Name", "Someone Example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a tailnet identity header authenticated a request", rec.Code)
	}
}

func TestAllowHostBindIsStillLoopbackOnly(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	for _, addr := range []string{"0.0.0.0:7778", "192.0.2.10:7778", "localhost:7778", "[::1]:7778"} {
		if _, err := NewServer(ServerOptions{
			Addr: addr, AllowHost: testPublishedHost, Devices: reg, Pairings: reg, Reader: newStubReader(), Writer: newStubWriter(), Captures: NewReservationStore(t.TempDir()),
		}); !errors.Is(err, ErrNotLoopback) {
			t.Fatalf("NewServer(Addr=%q, AllowHost set) error = %v, want ErrNotLoopback", addr, err)
		}
	}
}

func TestAllowHostIsValidatedAtStartup(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	for _, tc := range []struct {
		name      string
		allowHost string
	}{
		{"a scheme", "https://" + testPublishedHost},
		{"a path", testPublishedHost + "/v1/companion/today"},
		{"a wildcard", "*." + testPublishedHost},
		{"a list", testPublishedHost + "," + "other.example"},
		{"whitespace", " " + testPublishedHost},
		{"an embedded space", testPublishedHost + " other.example"},
		{"userinfo", "user@" + testPublishedHost},
		{"the expose placeholder, pasted unedited", "<your-node>.<your-tailnet>.ts.net"},
		{"the loopback host it already accepts", "127.0.0.1"},
		{"the loopback host with a port", "127.0.0.1:7778"},
		{"a port with no host", ":8443"},
		{"a non-numeric port", testPublishedHost + ":https"},
		{"a non-ASCII name", "n\u00f8de.tailnet-example.example"},
		{"a control character", testPublishedHost + "\n"},
		// The judge's exact input. The grammar below is an ALLOWLIST of RFC 1123
		// host syntax, so a shell metacharacter is refused because it is not a
		// hostname character, not because it appears on a list of bad ones.
		{"a shell command", "node.example;id"},
		{"a backtick", "node`id`.example"},
		{"command substitution", "node$(id).example"},
		{"a pipe", "node|id.example"},
		{"an ampersand", "node&id.example"},
		{"a newline in the middle", "node\nid.example"},
		{"a quote", "node'id'.example"},
		{"an underscore", "no_de.example"},
		{"a label starting with a hyphen", "-node.example"},
		{"a label ending with a hyphen", "node-.example"},
		{"a leading dot", "." + testPublishedHost},
		{"a trailing dot", testPublishedHost + "."},
		{"a doubled dot", "node..example"},
		{"a label over 63 characters", strings.Repeat("a", 64) + ".example"},
		{"a name over 253 characters", strings.TrimSuffix(strings.Repeat("abcdefgh.", 29), ".") + ".example"},
		{"port zero", testPublishedHost + ":0"},
		{"a port above the range", testPublishedHost + ":70000"},
		{"a bare colon", testPublishedHost + ":"},
		{"two ports", testPublishedHost + ":80:80"},
		{"an unclosed IPv6 literal", "[2001:db8::1:8443"},
		{"an IPv4 address in brackets", "[192.0.2.10]:8443"},
		{"a bracketed non-address", "[not-an-address]:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewServer(ServerOptions{
				Addr: "127.0.0.1:7778", AllowHost: tc.allowHost, Devices: reg, Pairings: reg, Reader: newStubReader(), Writer: newStubWriter(), Captures: NewReservationStore(t.TempDir()),
			})
			if !errors.Is(err, ErrBadAllowHost) {
				t.Fatalf("NewServer(AllowHost=%q) error = %v, want ErrBadAllowHost", tc.allowHost, err)
			}
		})
	}

	// And the shapes that must keep working.
	for _, ok := range []string{
		testPublishedHost,
		testPublishedHost + ":8443",
		testPublishedHost + ":1",
		testPublishedHost + ":65535",
		"[2001:db8::1]:8443",
		"[2001:db8::1]",
		"a.example",
		"node-1.sub-net.example",
		strings.Repeat("a", 63) + ".example",
	} {
		if _, err := NewServer(ServerOptions{
			Addr: "127.0.0.1:7778", AllowHost: ok, Devices: reg, Pairings: reg, Reader: newStubReader(), Writer: newStubWriter(), Captures: NewReservationStore(t.TempDir()),
		}); err != nil {
			t.Fatalf("NewServer(AllowHost=%q) refused a valid host: %v", ok, err)
		}
	}
}

func TestAllowHostLogsNeitherTheNameNorTheRequest(t *testing.T) {
	srv, token, log := allowHostServer(t, testPublishedHost)
	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, proxied(testPublishedHost, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, proxied("attacker.example", token))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	// Neither the served request nor the refused one may leave a trace: a
	// refusal that logs the Host it refused hands an attacker a way to write
	// arbitrary text into the operator's terminal.
	if got := log.String(); got != "" {
		t.Fatalf("the listener logged something per request:\n%s", got)
	}
}

func TestAllowHostBannerNamesThePropertyAndNotTheHost(t *testing.T) {
	// The banner is the one thing this listener writes. It says an extra host is
	// accepted, and it does NOT say which: the operator typed it, and a banner
	// without it is a log that can be pasted into a bug report unredacted.
	reg, _, _, _ := testRegistry(t)
	// Serve writes the banner from another goroutine, so the buffer the test
	// polls has to be synchronized or -race reports the test's own read.
	log := &lockedBuffer{}
	srv, err := NewServer(ServerOptions{
		Addr: "127.0.0.1:0", AllowHost: testPublishedHost, Devices: reg, Pairings: reg, Reader: newStubReader(), Writer: newStubWriter(), Captures: NewReservationStore(t.TempDir()), Log: log,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), "loopback peer") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	banner := log.String()
	if !strings.Contains(banner, "loopback peer") {
		t.Fatalf("the banner does not say a proxied request must come from a loopback peer:\n%s", banner)
	}
	if strings.Contains(banner, testPublishedHost) {
		t.Fatalf("the banner carries the published host name:\n%s", banner)
	}
}

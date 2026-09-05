package mora

// companion_confirm_url_test.go pins the derivation of the confirmation URL
// `mora companion pair` and `mora companion expose` publish.
//
// The defect this exists to prevent is a URL that is not mounted anywhere.
// Appending the route to the endpoint produced
// `https://host/v1/companion/v1/companion/pairing/confirm` for the endpoint
// shape the canonical pairing golden carries — and a phone that followed it
// would get a 404 from a route table that is working correctly, with no way to
// tell that from a Mac running an older build.

import (
	"net/url"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/companion"
)

// TestCompanionConfirmURLMountsTheRouteExactlyOnce drives the derivation over
// every endpoint shape the contract admits.
//
// The assertion is not a string comparison alone: each result is parsed, and its
// PATH must equal companion.RouteConfirm exactly. That is the property that
// matters — the route table is a list of absolute patterns, so anything the
// endpoint's own path contributes is a path that is not mounted.
func TestCompanionConfirmURLMountsTheRouteExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		want     string
	}{
		{
			// The shape the canonical pairing golden carries, and the one an
			// operator copies out of a QR payload. This is the case that was
			// broken.
			name:     "the canonical golden's endpoint",
			endpoint: "https://mora-mac.tail-scale.ts.net/v1/companion",
			want:     "https://mora-mac.tail-scale.ts.net" + companion.RouteConfirm,
		},
		{
			name:     "the same endpoint with a trailing slash",
			endpoint: "https://mora-mac.tail-scale.ts.net/v1/companion/",
			want:     "https://mora-mac.tail-scale.ts.net" + companion.RouteConfirm,
		},
		{
			// A bare origin: what `pair` defaults to.
			name:     "a bare loopback host with a port",
			endpoint: "http://127.0.0.1:7778",
			want:     "http://127.0.0.1" + ":7778" + companion.RouteConfirm,
		},
		{
			name:     "a bare host with a trailing slash",
			endpoint: "https://mora-mac.tail-scale.ts.net/",
			want:     "https://mora-mac.tail-scale.ts.net" + companion.RouteConfirm,
		},
		{
			name:     "a published origin with a non-default port",
			endpoint: "https://mora-mac.tail-scale.ts.net:8443/v1/companion",
			want:     "https://mora-mac.tail-scale.ts.net:8443" + companion.RouteConfirm,
		},
		{
			// A bracketed IPv6 literal. url.Host keeps the brackets, so the
			// origin round-trips as a valid authority rather than as a bare
			// address with colons in it.
			name:     "a bracketed IPv6 literal with a port",
			endpoint: "http://[::1]:7778/v1/companion",
			want:     "http://[::1]:7778" + companion.RouteConfirm,
		},
		{
			name:     "a bracketed IPv6 literal with no port",
			endpoint: "https://[2001:db8::1]/v1/companion",
			want:     "https://[2001:db8::1]" + companion.RouteConfirm,
		},
		{
			// The route itself, handed back in. Idempotent: deriving twice must
			// not stack the path, because `expose` derives from a URL `pair`
			// may already have derived.
			name:     "an endpoint that is already the confirm URL",
			endpoint: "https://mora-mac.tail-scale.ts.net" + companion.RouteConfirm,
			want:     "https://mora-mac.tail-scale.ts.net" + companion.RouteConfirm,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := companionConfirmURL(tc.endpoint)
			if got != tc.want {
				t.Fatalf("companionConfirmURL(%q) = %q, want %q", tc.endpoint, got, tc.want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("the derived URL does not parse: %v", err)
			}
			if u.Path != companion.RouteConfirm {
				t.Fatalf("path = %q, want exactly %q — the route is mounted once, from the origin",
					u.Path, companion.RouteConfirm)
			}
			if strings.Count(got, "/v1/companion") != 1 {
				t.Fatalf("%q names the companion namespace %d times, want 1",
					got, strings.Count(got, "/v1/companion"))
			}
		})
	}
}

// TestCompanionPairEmitsAUsableConfirmURL drives the whole command, because the
// derivation is only worth anything if it is what the payload actually carries.
func TestCompanionPairEmitsAUsableConfirmURL(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	// The default endpoint: the loopback listener.
	doc := decodeCompanion(t, run(t, "companion", "pair", "--json"), schemaCompanionPair)
	endpoint, _ := doc["endpoint"].(string)
	confirm, _ := doc["confirm_url"].(string)
	if confirm == "" {
		t.Fatal("pair emitted no confirm_url; a phone would have to guess the path")
	}
	assertConfirmURL(t, confirm, endpoint)

	// And with the endpoint shape the golden carries, supplied explicitly.
	doc = decodeCompanion(t, run(t, "companion", "pair", "--json",
		"--endpoint", "https://mora-mac.tail-scale.ts.net/v1/companion"), schemaCompanionPair)
	confirm, _ = doc["confirm_url"].(string)
	want := "https://mora-mac.tail-scale.ts.net" + companion.RouteConfirm
	if confirm != want {
		t.Fatalf("confirm_url = %q, want %q", confirm, want)
	}

	// The human rendering carries it too, because the operator reading a
	// terminal is the one who has to tell the phone where to post.
	human := run(t, "companion", "pair", "--endpoint", "https://mora-mac.tail-scale.ts.net/v1/companion")
	if !strings.Contains(human, want) {
		t.Fatalf("the human rendering does not carry the confirm URL:\n%s", human)
	}
}

// assertConfirmURL checks that a published confirm URL is the endpoint's origin
// with the route mounted once.
func assertConfirmURL(t *testing.T, confirm, endpoint string) {
	t.Helper()
	c, err := url.Parse(confirm)
	if err != nil {
		t.Fatalf("confirm_url %q does not parse: %v", confirm, err)
	}
	e, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("endpoint %q does not parse: %v", endpoint, err)
	}
	if c.Scheme != e.Scheme || c.Host != e.Host {
		t.Fatalf("confirm_url origin %s://%s does not match the endpoint's %s://%s",
			c.Scheme, c.Host, e.Scheme, e.Host)
	}
	if c.Path != companion.RouteConfirm {
		t.Fatalf("confirm_url path = %q, want exactly %q", c.Path, companion.RouteConfirm)
	}
}

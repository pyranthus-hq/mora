package imessage

import "testing"

// TestHandleResolution proves the AddressBook handle→name contract (IMSG-04 / D-09):
// a matching contact resolves to its name, phone/email handles normalize so
// equivalent forms hit the same contact, and a NO-match handle falls back to the RAW
// handle verbatim — never a fabricated "Unknown", never blank. It seeds the resolver
// with a known in-memory map (no live DB needed for the unit gate).
func TestHandleResolution(t *testing.T) {
	r := newResolverFromMap(map[string]string{
		"+14155551234":      "Neil Patel",
		"someone@email.com": "Sam Iam",
	})

	cases := []struct {
		name   string
		handle string
		want   string
	}{
		{"phone exact match", "+14155551234", "Neil Patel"},
		{"phone formatted match (strip non-digits)", "+1 (415) 555-1234", "Neil Patel"},
		{"phone no plus, same digits", "14155551234", "Neil Patel"},
		{"email exact match", "someone@email.com", "Sam Iam"},
		{"email case-insensitive match", "Someone@Email.com", "Sam Iam"},
		{"phone no match → raw handle", "+19998887777", "+19998887777"},
		{"email no match → raw handle", "nobody@nowhere.io", "nobody@nowhere.io"},
		{"opaque handle no match → raw handle", "iMessage;-;weird", "iMessage;-;weird"},
		{"empty handle → empty (no fabrication)", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Resolve(tc.handle)
			if got != tc.want {
				t.Fatalf("Resolve(%q) = %q, want %q", tc.handle, got, tc.want)
			}
		})
	}
}

// TestResolverNoFabricatedUnknown guards D-09 at the API level: a resolver built from
// an EMPTY map (the degraded "AddressBook unreadable" case) resolves every handle to
// itself — all raw handles, which is correct, not a failure.
func TestResolverNoFabricatedUnknown(t *testing.T) {
	r := newResolverFromMap(nil)
	for _, h := range []string{"+14155551234", "x@y.com", "iMessage;-;abc"} {
		if got := r.Resolve(h); got != h {
			t.Fatalf("empty resolver Resolve(%q) = %q, want raw handle %q", h, got, h)
		}
	}
}

// TestNewResolverMissingDBDegrades proves an unreadable/missing AddressBook root
// yields a USABLE (empty) resolver and never a fatal error — ingest must continue
// with all-raw-handles (D-09), never abort because contacts could not be read.
func TestNewResolverMissingDBDegrades(t *testing.T) {
	r, err := NewResolver("/nonexistent/addressbook/root")
	if err != nil {
		t.Fatalf("NewResolver on a missing root returned error %v, want graceful empty resolver", err)
	}
	if r == nil {
		t.Fatal("NewResolver returned nil resolver; want a usable empty resolver")
	}
	if got := r.Resolve("+14155551234"); got != "+14155551234" {
		t.Fatalf("missing-DB resolver should fall back to raw handle, got %q", got)
	}
}

// TestResolverLookup distinguishes an Address Book name from the raw-handle
// fallback without changing Resolve's compatibility contract.
func TestResolverLookup(t *testing.T) {
	resolved := newResolverFromMap(map[string]string{
		"+14155551234":      "Neil Patel",
		"empty@example.com": "",
	})
	cases := []struct {
		name     string
		resolver *Resolver
		handle   string
		want     string
		wantOK   bool
	}{
		{name: "resolved", resolver: resolved, handle: "+1 (415) 555-1234", want: "Neil Patel", wantOK: true},
		{name: "unresolved", resolver: resolved, handle: "+19998887777", want: "", wantOK: false},
		{name: "empty handle", resolver: resolved, handle: "", want: "", wantOK: false},
		{name: "nil resolver", resolver: nil, handle: "+14155551234", want: "", wantOK: false},
		{name: "empty mapping", resolver: resolved, handle: "empty@example.com", want: "", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, gotOK := tc.resolver.Lookup(tc.handle)
			if got != tc.want || gotOK != tc.wantOK {
				t.Fatalf("Lookup(%q) = (%q, %t), want (%q, %t)", tc.handle, got, gotOK, tc.want, tc.wantOK)
			}
		})
	}
}

package identity

import "testing"

func TestIdentityNormalization(t *testing.T) {
	cases := map[string]string{" A@EXAMPLE.COM ": "a@example.com", " +123 ": "+123", "": ""}
	for raw, want := range cases {
		kind := "handle"
		if raw == " A@EXAMPLE.COM " {
			kind = "address"
		}
		if got := Normalize(kind, raw); got != want {
			t.Errorf("Normalize(%q)=%q want %q", raw, got, want)
		}
	}
	mail := map[string]string{" First.Last+tag@GoogleMail.com ": "firstlast@gmail.com", "plain@example.com": "plain@example.com", "handle": "handle"}
	for raw, want := range mail {
		if got := MailboxKey(raw); got != want {
			t.Errorf("MailboxKey(%q)=%q want %q", raw, got, want)
		}
	}
	tokens := SelfNameTokens(map[string]bool{"Adit.Karode+work@example.com": true, "x@example.com": true, "plain_user": true})
	if !tokens["adit"] || !tokens["karode"] || !tokens["work"] || !tokens["plain"] || !tokens["user"] || tokens["x"] {
		t.Fatalf("tokens=%v", tokens)
	}

}

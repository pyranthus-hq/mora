package mora

import (
	"bytes"
	"strings"
	"testing"
)

// The `mora connect google` flow opens an OAuth consent screen against the
// shared "Mora" app, which is not (yet) through Google's restricted-scope
// verification. Google therefore shows a "Google hasn't verified this app"
// warning before the consent screen. A cautious user — exactly the kind a
// privacy tool attracts — bounces there unless the CLI tells them, up front,
// (1) that the warning is expected and the exact path past it, and (2) that they
// can use their own Google credentials instead. This copy is the only place that
// promise is made before the browser opens, so guard it.
func TestGoogleAuthPreambleSetsVerificationAndBYOExpectations(t *testing.T) {
	var buf bytes.Buffer
	printGoogleAuthPreamble(&buf)
	out := buf.String()

	for _, want := range []string{
		"hasn't verified",         // the exact phrase Google shows on the warning screen
		"Advanced",                // the button users must click to get past it
		"MORA_GOOGLE_CREDENTIALS", // the bring-your-own-credentials opt-out
	} {
		if !strings.Contains(out, want) {
			t.Errorf("connect-google preamble does not mention %q, so the user hits Google's warning unprepared\n--- got ---\n%s", want, out)
		}
	}
}

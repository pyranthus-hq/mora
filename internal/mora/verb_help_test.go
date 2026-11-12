package mora

import (
	"bytes"
	"strings"
	"testing"
)

// `mora <verb> --help` prints that verb's usage and succeeds. It used to fail
// with "flag: help requested".
func TestVerbHelpPrintsUsageAndSucceeds(t *testing.T) {
	withTempHome(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"read", "--help"}, "usage for mora read:"},
		{[]string{"read", "-h"}, "mora read <id>"},
		{[]string{"backup", "--help"}, "usage: mora backup [flags]"},
		{[]string{"hook", "install", "--help"}, "usage for mora hook install:"},
		{[]string{"setup", "--help"}, "mora setup --plan"},
		{[]string{"write", "--help"}, "usage for mora write:"},
	}
	for _, tc := range cases {
		var out, errb bytes.Buffer
		if err := Run(testCtx(t), tc.args, &out, &errb, strings.NewReader("")); err != nil {
			t.Errorf("%v: err = %v", tc.args, err)
			continue
		}
		if !strings.Contains(out.String(), tc.want) || strings.Contains(out.String(), "help requested") || errb.Len() != 0 {
			t.Errorf("%v: stdout=%q stderr=%q, want %q", tc.args, out.String(), errb.String(), tc.want)
		}
	}
	// A real usage mistake still fails.
	var out, errb bytes.Buffer
	if err := Run(testCtx(t), []string{"read", "--no-such-flag"}, &out, &errb, strings.NewReader("")); err == nil {
		t.Error("an unknown flag must still fail")
	}
}

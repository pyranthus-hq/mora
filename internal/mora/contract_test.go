package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func runSplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	err = Run(context.Background(), args, &outBuf, &errBuf, strings.NewReader(""))
	return outBuf.String(), errBuf.String(), err
}

func TestContractTracerLint(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "json receipt", args: []string{"lint", "--json"}},
		{name: "unknown flag", args: []string{"lint", "--json", "--bogusflag"}},
		{name: "human output", args: []string{"lint"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runSplit(t, tc.args...)
			switch tc.name {
			case "json receipt":
				if err != nil {
					t.Fatal(err)
				}
				var receipt map[string]any
				if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
					t.Fatalf("lint JSON does not decode as one document: %v", err)
				}
				if got := receipt["schema"]; got != "mora.lint.report" {
					t.Fatalf("schema = %#v", got)
				}
				if got := receipt["schema_version"]; got != float64(1) {
					t.Fatalf("schema_version = %#v", got)
				}
				issues, ok := receipt["issues"].([]any)
				if !ok || issues == nil {
					t.Fatalf("issues = %#v, want non-nil array", receipt["issues"])
				}
				if json.Valid([]byte(stderr)) {
					t.Fatalf("stderr unexpectedly contains JSON: %q", stderr)
				}
			case "unknown flag":
				if err == nil {
					t.Fatal("lint --json --bogusflag returned nil error")
				}
				var moraErr moraError
				if !errors.As(err, &moraErr) {
					t.Fatalf("error %T does not expose moraError", err)
				}
				if moraErr.Code != errCodeUsageUnknownFlag {
					t.Fatalf("error code = %q", moraErr.Code)
				}
				if stdout != "" {
					t.Fatalf("stdout = %q, want empty", stdout)
				}
			case "human output":
				if err != nil {
					t.Fatal(err)
				}
				if json.Valid([]byte(stdout)) {
					t.Fatalf("human lint output unexpectedly decodes as JSON: %q", stdout)
				}
			}
		})
	}
}

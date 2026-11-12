package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// A failed read is an error in the report, never "nothing waits for review".
func TestStatusSurfacesAFailedRead(t *testing.T) {
	cfg := projectBriefSetup(t)
	t.Chdir(t.TempDir())
	// A regular file where the proposal directory belongs makes the review
	// listing fail with an error other than "does not exist".
	dir := mcppkg.ProposalDir(cfg)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	text := run(t, "status")
	if strings.Contains(text, "nothing waits for review") || !strings.Contains(text, "Error:") || !strings.Contains(text, "could not read review") ||
		!strings.Contains(text, "Review:  unknown") {
		t.Fatalf("status text hides the failed read:\n%s", text)
	}

	out := run(t, "status", "--json")
	var env struct {
		Schema string        `json:"schema"`
		Errors []statusError `json:"errors"`
		Review statusReview  `json:"review"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if env.Schema != "mora.status" || len(env.Errors) != 1 || env.Errors[0].Part != "review" || env.Errors[0].Error == "" {
		t.Fatalf("status --json errors = %+v\n%s", env.Errors, out)
	}

	// With nothing wrong the key is absent, so the contract shape is unchanged.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	var healthy map[string]any
	out = run(t, "status", "--json")
	if err := json.Unmarshal([]byte(out), &healthy); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if _, has := healthy["errors"]; has {
		t.Fatalf("a healthy report carries an errors key:\n%s", out)
	}
	if text = run(t, "status"); !strings.Contains(text, "nothing waits for review") || strings.Contains(text, "Error:") {
		t.Fatalf("healthy status text:\n%s", text)
	}
}

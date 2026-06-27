package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// doctor --json must emit a machine-readable health report whose `healthy`
// field reflects the critical checks, so a release regression harness can gate
// on it instead of scraping human text.
func TestDoctorJSONReportsHealthy(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "personal", "--type", "fact",
		"--title", "seed", "--text", "Northwind pilot kicked off")
	run(t, "index", "rebuild")

	out := run(t, "doctor", "--json")
	var rep struct {
		Healthy bool `json:"healthy"`
		Checks  []struct {
			Name     string `json:"name"`
			OK       bool   `json:"ok"`
			Critical bool   `json:"critical"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("doctor --json must emit JSON: %v\noutput:\n%s", err, out)
	}
	if !rep.Healthy {
		t.Fatalf("freshly seeded vault must report healthy; got: %s", out)
	}
	// The critical floor (vault + index_db) must be present, marked critical, and green.
	want := map[string]bool{"vault": false, "index_db": false}
	for _, c := range rep.Checks {
		if _, tracked := want[c.Name]; !tracked {
			continue
		}
		if !c.Critical {
			t.Fatalf("check %q must be marked critical", c.Name)
		}
		if !c.OK {
			t.Fatalf("critical check %q must be ok on a seeded vault", c.Name)
		}
		want[c.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("doctor --json missing critical check %q", name)
		}
	}
}

// doctor --strict must exit non-zero (Run returns an error) when a critical
// check fails, so the harness fails loud — while default `mora doctor` stays
// exit-0 even when unhealthy (backward-compatible).
func TestDoctorStrictErrorsWhenUnhealthy(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "personal", "--type", "fact",
		"--title", "seed", "--text", "Northwind pilot kicked off")
	run(t, "index", "rebuild")

	// Healthy vault: --strict succeeds.
	if err := Run(context.Background(), []string{"doctor", "--strict"},
		io.Discard, io.Discard, strings.NewReader("")); err != nil {
		t.Fatalf("doctor --strict on a healthy vault must succeed: %v", err)
	}

	// Break a critical check: remove the index DB.
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dbPath(cfg)); err != nil {
		t.Fatalf("remove index db: %v", err)
	}

	// --strict must now error.
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"doctor", "--strict"},
		&out, &out, strings.NewReader("")); err == nil {
		t.Fatalf("doctor --strict must error when a critical check fails; output:\n%s", out.String())
	}

	// Default doctor must STILL exit 0 even when unhealthy (no behavior change).
	if err := Run(context.Background(), []string{"doctor"},
		io.Discard, io.Discard, strings.NewReader("")); err != nil {
		t.Fatalf("default `mora doctor` must stay exit-0 even when unhealthy: %v", err)
	}
}

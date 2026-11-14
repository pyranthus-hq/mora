package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// A plain `mora write` from inside an agent session is an agent write: it
// saves tentative, like write_memory under the default policy, and without an
// agent variable nothing changes. TestMain clears the variables for the
// package, so each case here sets its own.

var agentWriteNote = []string{"--scope", "project:demo", "--type", "fact",
	"--title", "Quokka owner", "--text", "Mallory owns the quokka deployment pipeline."}

type agentWriteReceipt struct {
	ID    string `json:"id"`
	Trust string `json:"trust"`
}

func agentWriteRecall(t *testing.T, bound string) string {
	t.Helper()
	var out bytes.Buffer
	in := `{"prompt":"who owns the quokka deployment pipeline","cwd":` + quoteJSON(bound) + `}`
	if err := cmdHook(testCtx(t), []string{"recall"}, &out, testStderr, strings.NewReader(in)); err != nil {
		t.Fatalf("hook recall: %v", err)
	}
	return out.String()
}

func TestWriteInAgentSessionSavesAnUnreviewedAgentNote(t *testing.T) {
	for _, key := range rulesAgentEnv {
		subRun(t, key, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			bound := rsDir(t)
			run(t, "project", "bind", "--scope", "project:demo", bound)
			t.Setenv(key, "1")

			stdout, stderr, err := mineRun(t, nil, append(append([]string{"write"}, agentWriteNote...), "--json")...)
			if err != nil {
				t.Fatalf("write: %v\n%s", err, stderr)
			}
			if n := strings.Count(stderr, "\n"); n != 1 || !strings.Contains(stderr, "saved as an unreviewed agent note") || !strings.Contains(stderr, key) {
				t.Fatalf("want one stderr line naming the unreviewed agent note, got %q", stderr)
			}
			var r agentWriteReceipt
			if err := json.Unmarshal([]byte(stdout), &r); err != nil || r.ID == "" || r.Trust != memory.TrustTentative {
				t.Fatalf("receipt = %s (%v); want trust %q like memory.trust of write_memory", stdout, err, memory.TrustTentative)
			}
			m, err := findMemory(cfg, r.ID)
			if err != nil || !memory.IsTentative(m) {
				t.Fatalf("note %s is not tentative: %+v (%v)", r.ID, m, err)
			}
			if logged, err := agentWriteLogged(cfg, r.ID); err != nil || !logged {
				t.Fatalf("no audit line for %s: %v %v", r.ID, logged, err)
			}
			clearAgentEnv(t)
			if out := run(t, "review", "--list"); !strings.Contains(out, r.ID+"\tsaved\t") {
				t.Fatalf("review --list misses the note:\n%s", out)
			}
			if got := agentWriteRecall(t, bound); strings.Contains(got, "quokka") {
				t.Fatalf("an unreviewed agent note reached recall:\n%s", got)
			}
		})
	}
}

func TestWriteWithoutAnAgentSessionIsUnchanged(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	bound := rsDir(t)
	run(t, "project", "bind", "--scope", "project:demo", bound)
	// Piped stdin is not an agent session: scripts and scheduled jobs write this way.
	stdout, stderr, err := mineRun(t, strings.NewReader("ignored\n"), append(append([]string{"write"}, agentWriteNote...), "--json")...)
	if err != nil {
		t.Fatalf("write: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "unreviewed") {
		t.Fatalf("stderr = %q", stderr)
	}
	if strings.Contains(stdout, `"trust"`) {
		t.Fatalf("a plain receipt carries trust: %s", stdout)
	}
	var r agentWriteReceipt
	if err := json.Unmarshal([]byte(stdout), &r); err != nil || r.ID == "" {
		t.Fatalf("receipt = %s (%v)", stdout, err)
	}
	m, err := findMemory(cfg, r.ID)
	if err != nil || memory.IsTentative(m) {
		t.Fatalf("a plain write became tentative: %+v (%v)", m, err)
	}
	if out := run(t, "review", "--list"); strings.Contains(out, r.ID) {
		t.Fatalf("a plain note is in the review list:\n%s", out)
	}
	if logged, err := agentWriteLogged(cfg, r.ID); err != nil || logged {
		t.Fatalf("a plain write left an agent audit line: %v %v", logged, err)
	}
	if got := agentWriteRecall(t, bound); !strings.Contains(got, "quokka") {
		t.Fatalf("a plain note is missing from recall:\n%s", got)
	}
}

func TestWriteMineIsStillRefusedInAnAgentSession(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	reviewTerminal(t)
	t.Setenv("CLAUDECODE", "1")
	_, _, err := mineRun(t, strings.NewReader(mineYes), mineArgs("write", "--mine")...)
	if !errors.Is(err, errOwnerVerificationUnavailable) {
		t.Fatalf("--mine in an agent session: %v", err)
	}
	if rows, err := listTentativeMemories(cfg); err != nil || len(rows) != 0 {
		t.Fatalf("a refused --mine wrote a note: %v %v", rows, err)
	}
}

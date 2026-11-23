package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func reviewAppCaller(t *testing.T, err error) {
	t.Helper()
	prev := verifyStatusAppCaller
	verifyStatusAppCaller = func() error { return err }
	t.Cleanup(func() { verifyStatusAppCaller = prev })
}

func reviewAppRows(t *testing.T) map[string]reviewRow {
	t.Helper()
	var got struct {
		Notes []reviewRow `json:"notes"`
	}
	if err := json.Unmarshal([]byte(run(t, "review", "--list", "--json")), &got); err != nil {
		t.Fatal(err)
	}
	rows := map[string]reviewRow{}
	for _, r := range got.Notes {
		rows[r.ID] = r
	}
	return rows
}

func reviewAppKeep(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(testCtx(t), append([]string{"review", "app-keep"}, args...), &out, &out, strings.NewReader(""))
	return out.String(), err
}

func TestReviewAppKeepOwnerConfirmsOnlyFromTheApp(t *testing.T) {
	cfg := reviewSetup(t)
	clearAgentEnv(t)
	saved := reviewSaved(t, "Saved note")
	held := reviewHeld(t, "Held note")
	rows := reviewAppRows(t)
	if rows[saved].Confirm == "" || len(rows[saved].Fields) == 0 || rows[held].Confirm == "" {
		t.Fatalf("list must carry the digest and its fields: %+v", rows)
	}

	reviewAppCaller(t, errors.New("the caller was not started by Mora Status"))
	if _, err := reviewAppKeep(t, "saved", saved, rows[saved].Confirm); err == nil || !strings.Contains(err.Error(), "Only the signed Mora Status app") {
		t.Fatalf("non-app caller err = %v", err)
	}
	reviewAppCaller(t, nil)
	t.Setenv("CLAUDECODE", "1")
	if _, err := reviewAppKeep(t, "saved", saved, rows[saved].Confirm); err == nil || !strings.Contains(err.Error(), "agent session") {
		t.Fatalf("agent session err = %v", err)
	}
	clearAgentEnv(t)
	if _, err := reviewAppKeep(t, "saved", saved, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "changed after it was shown") {
		t.Fatalf("stale digest err = %v", err)
	}
	if m, _ := findMemory(cfg, saved); memory.IsOwnerConfirmed(m) || !memory.IsTentative(m) {
		t.Fatal("a refused call changed the note")
	}
	if _, err := reviewAppKeep(t, "held", saved, rows[saved].Confirm); err == nil {
		t.Fatal("a saved id passed as held must fail")
	}

	if out, err := reviewAppKeep(t, "saved", saved, rows[saved].Confirm); err != nil || !strings.Contains(out, "owner-confirmed") {
		t.Fatalf("saved keep: %q %v", out, err)
	}
	if m, _ := findMemory(cfg, saved); !memory.IsOwnerConfirmed(m) || memory.IsTentative(m) {
		t.Fatal("saved note is not owner-confirmed")
	}
	out, err := reviewAppKeep(t, "held", held, rows[held].Confirm)
	if err != nil || !strings.Contains(out, "owner-confirmed memory") {
		t.Fatalf("held keep: %q %v", out, err)
	}
	if len(reviewAppRows(t)) != 0 {
		t.Fatal("kept notes still wait for review")
	}
}

func TestReviewAppKeepUsage(t *testing.T) {
	reviewSetup(t)
	reviewAppCaller(t, nil)
	for _, args := range [][]string{{}, {"saved", "x"}, {"other", "x", "d"}, {"saved", "x", ""}} {
		if _, err := reviewAppKeep(t, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

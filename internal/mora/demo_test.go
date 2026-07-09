package mora

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// demoConfigFor builds the isolated Config cmdDemo seeds at dir (mirrors the
// MORA_CONFIG_DIR rooting), for asserting against the seeded vault directly.
func demoConfigFor(dir string) Config {
	return Config{
		VaultDir:  filepath.Join(dir, "vault"),
		ConfigDir: dir,
		DataDir:   filepath.Join(dir, "data"),
		StateDir:  filepath.Join(dir, "state"),
	}
}

func seedDemo(t *testing.T) Config {
	t.Helper()
	t.Setenv("MORA_CONFIG_DIR", "") // hermetic: never let a developer's export leak the live install
	dir := filepath.Join(t.TempDir(), "mora-demo")
	var out bytes.Buffer
	if err := cmdDemo(context.Background(), []string{"--dir", dir, "--quiet"}, &out); err != nil {
		t.Fatalf("cmdDemo: %v", err)
	}
	return demoConfigFor(dir)
}

// TestDemoSeedsCrossSourceBrief is the load-bearing guarantee: the seeded vault
// renders a brief with a section PER SOURCE (Calendar/Texts/Emails/Files) plus
// open tasks — i.e. the flagship "one digest across every source" actually shows
// up, which a `mora write`-built vault could not produce.
func TestDemoSeedsCrossSourceBrief(t *testing.T) {
	cfg := seedDemo(t)

	d, err := briefDigest(cfg, time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("briefDigest: %v", err)
	}

	got := map[string]int{}
	for _, s := range d.Sections {
		got[s.Source] = len(s.Items)
	}
	for _, want := range []string{"calendar", "imessage", "gmail", "filesystem"} {
		if got[want] == 0 {
			t.Errorf("brief is missing a non-empty %q section; sections=%v", want, got)
		}
	}
	if len(d.StaleTasks) != 2 {
		t.Errorf("StaleTasks = %d, want 2 (the seeded open loops)", len(d.StaleTasks))
	}
}

// TestDemoUnifiedIdentity proves the headline differentiator: the showcased
// person (Priya Nair) resolves to ONE graph node spanning email + texts +
// calendar — not duplicated across sources — because her iMessage handle is her
// email, so the RULE-1 mailbox merge collapses all three.
func TestDemoUnifiedIdentity(t *testing.T) {
	cfg := seedDemo(t)

	paths, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatalf("allMemoryFiles: %v", err)
	}
	var mems []Memory
	for _, p := range paths {
		m, perr := parseMemory(p)
		if perr != nil {
			t.Fatalf("parseMemory(%s): %v", p, perr)
		}
		mems = append(mems, m)
	}
	ents, _, _ := buildGraph(mems)

	const want = "person:priya@northwind.com"
	var matches []graphEntity
	for _, e := range ents {
		if e.ID == want {
			matches = append(matches, e)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("found %d person nodes for %q, want exactly 1 (a duplicate means identity did not merge across sources)", len(matches), want)
	}
	p := matches[0]
	if p.Kind != "person" {
		t.Errorf("Kind = %q, want person", p.Kind)
	}
	if p.DisplayName != "Priya Nair" {
		t.Errorf("DisplayName = %q, want %q", p.DisplayName, "Priya Nair")
	}
	// She appears in 2 calendar + 2 imessage + 3 gmail memories; require evidence
	// from clearly more than one source so this can't pass on a single-source node.
	if p.MentionCount < 4 {
		t.Errorf("MentionCount = %d, want >= 4 (cross-source evidence)", p.MentionCount)
	}
}

// TestCleanDigestBody pins the --clean presentation transform: it removes the
// internal id suffix and the "+N more" counter, and leaves headings, prefixes,
// counts, and item text untouched (including a snippet that legitimately ends in
// a parenthetical, which must NOT be mistaken for an id).
func TestCleanDigestBody(t *testing.T) {
	in := "# Mora digest\n" +
		"\n## Emails — baseline (2)\n" +
		"- [new] Re: kickoff — agenda looks good (id: gmail_thread/abc123)\n" +
		"- [new] spec v2 — phase 1 scoped (two regions) (id: gmail_thread/def456)\n" +
		"- +7 more since last brief\n" +
		"\n## Open tasks (1 stale)\n" +
		"- Send Priya the addendum\n"
	got := cleanDigestBody(in)

	if strings.Contains(got, "(id: ") {
		t.Errorf("clean body still contains an id suffix:\n%s", got)
	}
	if strings.Contains(got, "more since last brief") {
		t.Errorf("clean body still contains the +N more counter:\n%s", got)
	}
	for _, keep := range []string{
		"## Emails — baseline (2)", // heading untouched
		"[new] Re: kickoff — agenda looks good",
		"[new] spec v2 — phase 1 scoped (two regions)", // legit trailing parenthetical survives
		"## Open tasks (1 stale)",
		"- Send Priya the addendum",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("clean body dropped expected content %q:\n%s", keep, got)
		}
	}
}

// TestCmdDemoRefusesDangerousDirs drives the real entry point with paths that
// equal, nest under, or symlink to the live install, and asserts cmdDemo refuses
// every one WITHOUT disturbing a sentinel in the live vault (the vault-flip
// failure class).
func TestCmdDemoRefusesDangerousDirs(t *testing.T) {
	t.Setenv("MORA_CONFIG_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	liveMem := filepath.Join(home, "vault", "mora", "memories")
	if err := os.MkdirAll(liveMem, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(liveMem, "real.md")
	if err := os.WriteFile(sentinel, []byte("REAL DATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	dangerous := []string{
		filepath.Join(home, ".config", "mora"),        // == live config
		filepath.Join(home, ".config", "mora", "sub"), // nested under live config
		filepath.Join(home, "vault", "mora"),          // == live vault
		filepath.Join(home, "vault", "mora", "sub"),   // nested under live vault
		home, // home itself
	}
	for _, d := range dangerous {
		var out bytes.Buffer
		if err := cmdDemo(context.Background(), []string{"--dir", d, "--quiet", "--force"}, &out); err == nil {
			t.Errorf("cmdDemo --dir %q succeeded; want refusal", d)
		}
	}

	// A demo dir whose vault is a SYMLINK to the live vault must be refused before
	// any write/remove.
	sneaky := filepath.Join(t.TempDir(), "sneaky")
	if err := os.MkdirAll(sneaky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "vault", "mora"), filepath.Join(sneaky, "vault")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdDemo(context.Background(), []string{"--dir", sneaky, "--quiet", "--force"}, &out); err == nil {
		t.Errorf("cmdDemo --dir %q (vault symlinked to live) succeeded; want refusal", sneaky)
	}

	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "REAL DATA" {
		t.Errorf("live vault sentinel was disturbed: err=%v content=%q", err, string(b))
	}
}

// TestCmdDemoForceRequiresMarker proves --force only ever overwrites a
// demo-owned directory: a populated non-demo dir is refused (and survives), and a
// genuine demo dir reseeds only with --force.
func TestCmdDemoForceRequiresMarker(t *testing.T) {
	t.Setenv("MORA_CONFIG_DIR", "")
	t.Setenv("HOME", t.TempDir())

	// A populated dir without the demo marker is someone's real data.
	notDemo := filepath.Join(t.TempDir(), "notdemo")
	mem := filepath.Join(notDemo, "vault", "memories")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(mem, "x.md")
	if err := os.WriteFile(keep, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdDemo(context.Background(), []string{"--dir", notDemo, "--quiet", "--force"}, &out); err == nil {
		t.Error("cmdDemo --force overwrote a non-demo populated dir; want refusal")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("non-demo data was removed by --force: %v", err)
	}

	// A genuine demo dir: first seed ok; reseed needs --force.
	demo := filepath.Join(t.TempDir(), "demo")
	if err := cmdDemo(context.Background(), []string{"--dir", demo, "--quiet"}, &out); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if err := cmdDemo(context.Background(), []string{"--dir", demo, "--quiet"}, &out); err == nil {
		t.Error("re-seed without --force succeeded; want refusal")
	}
	if err := cmdDemo(context.Background(), []string{"--dir", demo, "--quiet", "--force"}, &out); err != nil {
		t.Errorf("re-seed with --force on a demo dir failed: %v", err)
	}
}

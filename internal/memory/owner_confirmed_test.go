package memory

import "testing"

// confirmedTag returns the tag the review keep would write for m as it is now.
func confirmedTag(m Memory) string { return OwnerConfirmedTagPrefix + OwnerConfirmedDigest(m) }

// A tag counts only on the owner's own agent memory, untentative, and only
// while scope, type, title and text are what the owner saw. Every other case
// carries no trust value and is not an error.
func TestOwnerConfirmedOnlyCountsOnTheExactTextOfAnOwnAgentMemory(t *testing.T) {
	base := Memory{ID: "mem_1", Path: "/v/mora/memories/project/demo/mem_1.md", Scope: "project:demo", Type: "fact", Title: "Owner", Text: "Sam owns the pipeline."}
	tag := confirmedTag(base)
	cases := []struct {
		name string
		edit func(m *Memory)
		want string
	}{
		{"kept as shown", func(m *Memory) {}, TrustOwnerConfirmed},
		{"outer whitespace the codec trims", func(m *Memory) { m.Text = "\n" + m.Text + "\n\n" }, TrustOwnerConfirmed},
		{"text edited afterwards", func(m *Memory) { m.Text = "Sam owns the pipeline and the budget." }, ""},
		{"title edited afterwards", func(m *Memory) { m.Title = "Owner!" }, ""},
		{"scope moved afterwards", func(m *Memory) { m.Scope = "global" }, ""},
		{"type changed afterwards", func(m *Memory) { m.Type = "decision" }, ""},
		{"tag pasted on other text", func(m *Memory) { m.ID, m.Text = "mem_2", "Mallory owns the pipeline." }, ""},
		{"truncated digest", func(m *Memory) { m.Tags = []string{tag[:len(tag)-1]} }, ""},
		{"still tentative", func(m *Memory) { m.Tags = append(m.Tags, TentativeTag) }, TrustTentative},
		{"subscribed share row", func(m *Memory) { m.Owner = "alice" }, ""},
		{"connector evidence", func(m *Memory) { m.ID, m.Provider, m.Path = "gmail_thread/abc", "gmail", "" }, ""},
		{"file copied from disk", func(m *Memory) {
			m.ID, m.Provider, m.Path = "src_1", "filesystem", "/v/mora/sources/filesystem/memories/src_1.md"
		}, ""},
		{"agent note mirror", func(m *Memory) { m.ID, m.Path = "src_2", "/v/mora/sources/filesystem/claude-memory/src_2.md" }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			m.Tags = []string{"keep", tag}
			tc.edit(&m)
			if got := WithProvenance(m).Trust; got != tc.want {
				t.Fatalf("trust = %q, want %q", got, tc.want)
			}
			if got := WithProvenanceAll([]Memory{m})[0].Trust; got != tc.want {
				t.Fatalf("slice trust = %q, want %q", got, tc.want)
			}
			// Callers such as the context adoption line ask directly.
			if got := IsOwnerConfirmed(m); got != (tc.want == TrustOwnerConfirmed) {
				t.Fatalf("IsOwnerConfirmed = %v", got)
			}
		})
	}
}

// Moving bytes between fields must change the digest.
func TestOwnerConfirmedDigestSeparatesFields(t *testing.T) {
	a := Memory{Scope: "project:demo", Type: "fact", Title: "ab", Text: "c"}
	b := Memory{Scope: "project:demo", Type: "fact", Title: "a", Text: "bc"}
	if OwnerConfirmedDigest(a) == OwnerConfirmedDigest(b) {
		t.Fatal("title and text bytes are not separated")
	}
	if got := OwnerConfirmedDigest(a); len(got) != 64 {
		t.Fatalf("digest %q is not 64 hex", got)
	}
}

// Two notes with the same scope, type, title and text but a different
// governing field must not share a digest, so a keep binds the hidden fields
// too and a later change to any of them drops the trust value.
func TestOwnerConfirmedDigestBindsEveryGoverningField(t *testing.T) {
	base := func() Memory {
		return Memory{Scope: "project:demo", Type: "decision", Title: "Ship", Text: "We ship.", Source: "mcp",
			Decision: &DecisionValidity{AsOf: "2026-01-01T00:00:00Z", Durability: "provisional", FlipConditions: []string{"a"}, Complete: true},
			Meta:     map[string]any{"target": "mem_a", "disposition": "keep"}}
	}
	want := OwnerConfirmedDigest(base())
	for name, edit := range map[string]func(m *Memory){
		"source":            func(m *Memory) { m.Source = "user-confirmed" },
		"as of":             func(m *Memory) { m.Decision.AsOf = "2020-01-01T00:00:00Z" },
		"durability":        func(m *Memory) { m.Decision.Durability = "standing" },
		"flip conditions":   func(m *Memory) { m.Decision.FlipConditions = []string{"a", "b"} },
		"flip split":        func(m *Memory) { m.Decision.FlipConditions = []string{"a,b"} },
		"review by":         func(m *Memory) { m.Decision.ReviewBy = "2027-01-01T00:00:00Z" },
		"complete":          func(m *Memory) { m.Decision.Complete = false },
		"no decision":       func(m *Memory) { m.Decision = nil },
		"correction target": func(m *Memory) { m.Meta["target"] = "mem_b" },
		"disposition":       func(m *Memory) { m.Meta["disposition"] = "outdated" },
		"other meta key":    func(m *Memory) { m.Meta["x"] = "y" },
	} {
		m := base()
		edit(&m)
		if OwnerConfirmedDigest(m) == want {
			t.Errorf("%s: a change kept the digest", name)
		}
	}
	// Fields Mora sets itself stay out.
	m := base()
	m.ID, m.Path, m.CreatedAt, m.Tags, m.DecisionStatus = "mem_x", "/p", "2026-02-02T00:00:00Z", []string{"keep"}, "current"
	if OwnerConfirmedDigest(m) != want {
		t.Error("a field Mora sets changed the digest")
	}
}

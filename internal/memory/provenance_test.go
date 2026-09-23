package memory

import "testing"

func TestDeriveProvenanceCallsConnectorRecordsEvidence(t *testing.T) {
	cases := []struct {
		name   string
		record Memory
	}{
		{"mail thread by id", Memory{ID: "gmail_thread/abc", Provider: "gmail", ProviderID: "abc"}},
		{"message chat by id", Memory{ID: "imessage_chat/abc", Provider: "imessage"}},
		{"calendar event by id", Memory{ID: "calendar_event/abc", Provider: "calendar"}},
		{"apple calendar event by id", Memory{ID: "applecal_event/abc", Provider: "applecal"}},
		{"whatsapp conversation by id", Memory{ID: "whatsapp_conversation/abc", Provider: "whatsapp"}},
		{"github issue by id", Memory{ID: "github_issue/abc", Provider: "github"}},
		{"provider with no id prefix", Memory{ID: "hydrated-row", Provider: "gmail"}},
		{"connector record known only by its path", Memory{ID: "att_1", Path: "/home/a/vault/mora/sources/imessage/att_1.md"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveProvenance(c.record); got != ProvenanceEvidence {
				t.Fatalf("got %q, want %q", got, ProvenanceEvidence)
			}
		})
	}
}

// A file the filesystem connector copied from disk is a document. It exists,
// but nothing on the record says who wrote it, and on this vault the ideation
// folders, the planning trees and the Obsidian agent-memory folders are all
// agent prose. So a document is never called evidence, whatever folder it is in.
func TestDeriveProvenanceCallsFilesCopiedFromDiskDocuments(t *testing.T) {
	cases := []struct {
		name   string
		record Memory
	}{
		{"ideation note by path and tag", Memory{ID: "src_1", Path: "/home/a/vault/mora/sources/filesystem/knowledge/src_1.md", Tags: []string{"filesystem", "knowledge"}}},
		{"planning tree by path only", Memory{ID: "src_2", Path: "/home/a/vault/mora/sources/filesystem/pyranthus/src_2.md"}},
		{"obsidian folder by tag only", Memory{ID: "src_3", Tags: []string{"filesystem", "obsidian-memory"}}},
		{"hydrated row with the filesystem provider", Memory{ID: "src_4", Provider: "filesystem"}},
		{"human-written note is still only a document", Memory{ID: "src_5", Path: "/home/a/vault/mora/sources/filesystem/obsidian-memory/src_5.md", Source: "/home/a/Obsidian Vault/6 - Zettelkasten/note.md"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveProvenance(c.record); got != ProvenanceDocument {
				t.Fatalf("got %q, want %q", got, ProvenanceDocument)
			}
		})
	}
}

func TestDeriveProvenanceCallsAgentNotesAuthored(t *testing.T) {
	cases := []struct {
		name   string
		record Memory
	}{
		{"memory written through the server", Memory{ID: "mem_1", Path: "/home/a/vault/mora/memories/global/mem_1.md", Source: "mcp"}},
		{"memory written from the command line", Memory{ID: "mem_2", Path: "/home/a/vault/mora/memories/personal/mem_2.md", Source: "manual"}},
		{"memory with no path at all", Memory{ID: "mem_3", Source: "codex"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveProvenance(c.record); got != ProvenanceAuthored {
				t.Fatalf("got %q, want %q", got, ProvenanceAuthored)
			}
		})
	}
}

// An agent's own notes mirrored onto disk sit in the connector tree, but they
// are still the agent writing about the user. Calling them evidence would let a
// note launder itself into a connector record.
func TestAgentNoteMirrorsStayAuthoredEvenInTheSourcesTree(t *testing.T) {
	for _, name := range []string{"claude-memory", "claude-project-memory", "claude-remember", "codex-memory"} {
		byTag := Memory{ID: "src_1", Path: "/home/a/vault/mora/sources/filesystem/" + name + "/src_1.md", Tags: []string{"filesystem", name}}
		if got := DeriveProvenance(byTag); got != ProvenanceAuthored {
			t.Errorf("%s tagged: got %q, want %q", name, got, ProvenanceAuthored)
		}
		byPath := Memory{ID: "src_2", Path: "/home/a/vault/mora/sources/filesystem/" + name + "/src_2.md"}
		if got := DeriveProvenance(byPath); got != ProvenanceAuthored {
			t.Errorf("%s by path: got %q, want %q", name, got, ProvenanceAuthored)
		}
	}
}

// The id is the one field no writer controls, so it decides first: a connector
// record stays evidence whatever tag it carries, and an agent memory is never
// evidence whatever provider a hand-edited frontmatter line claims.
func TestTheRecordIDDecidesBeforeAnyWriterControlledField(t *testing.T) {
	connector := Memory{ID: "gmail_thread/abc", Provider: "gmail", Tags: []string{"codex-memory"}}
	if got := DeriveProvenance(connector); got != ProvenanceEvidence {
		t.Errorf("connector with a mirror tag: got %q, want %q", got, ProvenanceEvidence)
	}
	forged := Memory{ID: "mem_1", Path: "/home/a/vault/mora/memories/global/mem_1.md", Provider: "gmail", Source: "mcp"}
	if got := DeriveProvenance(forged); got != ProvenanceAuthored {
		t.Errorf("agent memory with an injected provider: got %q, want %q", got, ProvenanceAuthored)
	}
	hydrated := Memory{ID: "mem_2", Provider: "gmail", Source: "mcp"}
	if got := DeriveProvenance(hydrated); got != ProvenanceAuthored {
		t.Errorf("hydrated agent memory with an injected provider: got %q, want %q", got, ProvenanceAuthored)
	}
	relative := Memory{ID: "src_1", Path: "sources/filesystem/codex-memory/src_1.md"}
	if got := DeriveProvenance(relative); got != ProvenanceAuthored {
		t.Errorf("relative mirror path: got %q, want %q", got, ProvenanceAuthored)
	}
}

// Nothing a writer puts on the record can change the derived kind.
func TestDerivationIgnoresAnyProvenanceAlreadyOnTheRecord(t *testing.T) {
	forged := Memory{ID: "mem_1", Source: "mcp", Provenance: "evidence"}
	if got := WithProvenance(forged); got.Provenance != ProvenanceAuthored {
		t.Fatalf("a forged field survived: %q", got.Provenance)
	}
}

func TestWithProvenanceAllMarksEveryRow(t *testing.T) {
	rows := []Memory{
		{ID: "mem_1", Source: "mcp"},
		{ID: "gmail_thread/abc", Provider: "gmail"},
		{ID: "src_1", Path: "/home/a/vault/mora/sources/filesystem/knowledge/src_1.md"},
	}
	got := WithProvenanceAll(rows)
	want := []string{ProvenanceAuthored, ProvenanceEvidence, ProvenanceDocument}
	for i, kind := range want {
		if got[i].Provenance != kind {
			t.Errorf("row %d: got %q, want %q", i, got[i].Provenance, kind)
		}
	}
}

// A source label that claims the user confirmed something does not change the
// kind. Mora has no way to check such a claim, so guessing at it would invent
// the very authority this field exists to withhold. The label is carried to the
// reader verbatim instead.
func TestASourceLabelClaimingConfirmationStaysAuthored(t *testing.T) {
	for _, label := range []string{
		"user-confirmed 2026-08-12",
		"User Correction after review",
		"user confirmation is pending",
		"not a user correction",
		"Sam direct instruction in Codex",
		"mcp",
	} {
		m := Memory{ID: "mem_1", Path: "/home/a/vault/mora/memories/global/mem_1.md", Source: label}
		if got := DeriveProvenance(m); got != ProvenanceAuthored {
			t.Errorf("source %q: got %q, want %q", label, got, ProvenanceAuthored)
		}
	}
}

// A connector record keeps its origin whatever its source label says.
func TestConnectorRecordsKeepTheirOriginWhateverTheLabelSays(t *testing.T) {
	m := Memory{ID: "gmail_thread/abc", Provider: "gmail", Source: "user-confirmed"}
	if got := DeriveProvenance(m); got != ProvenanceEvidence {
		t.Fatalf("got %q, want %q", got, ProvenanceEvidence)
	}
	d := Memory{ID: "src_1", Path: "/home/a/vault/mora/sources/filesystem/knowledge/src_1.md", Source: "user-confirmed"}
	if got := DeriveProvenance(d); got != ProvenanceDocument {
		t.Fatalf("got %q, want %q", got, ProvenanceDocument)
	}
}

package search

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// contextBlock is one entry in the assembled context, split into the three
// parts the budget has to treat differently: the title line, the warning lines
// that say where the entry came from, and the body the budget may cut.
//
// The split exists because a title whose provenance line has been cut off
// reads like settled fact, which is the failure this surface exists to
// prevent. So the title and its warning are written together or not at all.
type contextBlock struct {
	title   string
	warning string
	body    string
}

// header is the title and its warning, which always travel together.
func (b contextBlock) header() string { return b.title + b.warning }

// sourceLabelMaxRunes keeps a long free-text source label from eating the
// budget. Labels in the wild run to a full sentence.
const sourceLabelMaxRunes = 60

// BuildContext assembles static vault controls and selected memories within a deterministic budget.
func BuildContext(cfg config.Config, items []memory.Memory, budget int, hasQuery bool) string {
	if budget <= 0 {
		return ""
	}
	var wiki []contextBlock
	for _, rel := range []string{"index.md", "priority-map.md", "live-tasks.md", "heartbeat.md", "auto-resolver.md"} {
		path := filepath.Join(cfg.VaultDir, rel)
		// index.md is generated cache data. Prefer the state copy, but retain a
		// vault fallback for existing installations that have not rebuilt yet.
		if rel == "index.md" {
			if _, err := os.Stat(filepath.Join(cfg.StateDir, rel)); err == nil {
				path = filepath.Join(cfg.StateDir, rel)
			}
		}
		if body, err := os.ReadFile(path); err == nil {
			wiki = append(wiki, contextBlock{
				title:   fmt.Sprintf("\n# %s\n", rel),
				warning: "Provenance: a vault control file, not a memory; who wrote it is unchecked.\n",
				body:    string(body) + "\n",
			})
		}
	}
	var records []contextBlock
	for _, m := range items {
		var warning, body strings.Builder
		fmt.Fprintf(&warning, "%s\n", provenanceLine(m))
		if line := adoptionLine(m); line != "" {
			fmt.Fprintf(&warning, "%s\n", line)
		}
		if line := laterRelatedLine(m); line != "" {
			fmt.Fprintf(&warning, "%s\n", line)
		}
		for _, c := range m.ExplicitCorrections {
			fmt.Fprintf(&warning, "Explicit correction asserted by record %s (%s; origin: %s). Excerpt: %s\n", c.ID, c.CreatedAt, c.Provenance, c.Text)
			if c.Disposition != "" {
				fmt.Fprintf(&warning, "Writer's disposition for the prior record: %s; this does not establish truth or user adoption.\n", c.Disposition)
			}
		}
		if m.CorrectionOmitted {
			warning.WriteString("Additional correction context was omitted by this request's filters or the per-record cap; do not rely on the prior claim alone.\n")
		}
		if m.Decision != nil {
			fmt.Fprintf(&body, "Decision status: %s\nAs of: %s\nDurability: %s\nFlip conditions: %s\n", m.DecisionStatus, m.Decision.AsOf, m.Decision.Durability, strings.Join(m.Decision.FlipConditions, "; "))
			if m.Decision.ReviewBy != "" {
				fmt.Fprintf(&body, "Review by: %s\n", m.Decision.ReviewBy)
			}
		}
		fmt.Fprintf(&body, "%s\n", m.Text)
		records = append(records, contextBlock{
			title:   fmt.Sprintf("\n# %s\n", m.Title),
			warning: warning.String(),
			body:    body.String(),
		})
	}
	first, second := wiki, records
	if hasQuery {
		first, second = records, wiki
	}
	// The budget counts bytes. genericutil.TruncateRunes takes a byte ceiling
	// and only backs the cut up to a rune boundary, and every caller measures
	// what it got with len(), so the accounting here has to be in the same
	// unit. Counting runes here and cutting bytes there let a body with
	// multi-byte characters run past the ceiling.
	var out strings.Builder
	for _, group := range [][]contextBlock{first, second} {
		for _, block := range group {
			remaining := budget - out.Len()
			if remaining <= 0 {
				return out.String()
			}
			if len(block.header()) > remaining {
				// The title and its warning do not fit together. A caller that
				// gets an empty string treats the surface as broken, so the
				// first block still prints what it can — but it prints a
				// warning, never the title, because a title on its own is the
				// thing this surface must not hand a reader. The warning is
				// never cut: every hedge in it sits at the end, so half a
				// warning reads as more confident than the whole one. If the
				// full line will not fit, a short complete one goes out, and
				// if even that will not fit, nothing does.
				if out.Len() == 0 {
					if len(block.warning) <= remaining {
						out.WriteString(block.warning)
					} else if len(missingCorrectionWarning(block)) <= remaining {
						out.WriteString(missingCorrectionWarning(block))
					} else if len(shortProvenanceWarning) <= remaining {
						out.WriteString(shortProvenanceWarning)
					}
				}
				return out.String()
			}
			out.WriteString(block.header())
			body := genericutil.TruncateRunes(block.body, budget-out.Len())
			out.WriteString(body)
			if len(body) < len(block.body) {
				// The body was cut, so the budget is spent.
				return out.String()
			}
		}
	}
	return out.String()
}

// shortProvenanceWarning stands in when the real one will not fit. It says less
// but it is complete, which a truncated warning never is.
const shortProvenanceWarning = "Provenance: unchecked; read the record itself.\n"

const shortCorrectionWarning = "Provenance: unchecked. A linked correction was omitted by this budget; read the records before relying on the prior claim.\n"

func missingCorrectionWarning(block contextBlock) string {
	if strings.Contains(block.warning, "Explicit correction asserted") || strings.Contains(block.warning, "Additional correction context was omitted") {
		return shortCorrectionWarning
	}
	return shortProvenanceWarning
}

// provenanceLine says where the record came from, in the reader's language.
// It never calls a record true, and it says outright that repeating an agent's
// note adds nothing to the evidence behind it.
func provenanceLine(m memory.Memory) string {
	switch memory.DeriveProvenance(m) {
	case memory.ProvenanceEvidence:
		return "Provenance: evidence from " + evidenceOrigin(m) + "."
	case memory.ProvenanceDocument:
		return "Provenance: a file on disk" + documentOrigin(m) + ". The file exists; what it says is unchecked, and the file may itself be an agent's write-up."
	default:
		return "Provenance: agent-written" + sourceSuffix(m) + "; not checked against the user's words. Repeating it adds no evidence."
	}
}

// evidenceOrigin names the connector a record came from, falling back to
// honest vagueness rather than a guess.
func evidenceOrigin(m memory.Memory) string {
	if m.Provider != "" && m.Provider != "filesystem" {
		return m.Provider
	}
	return "a connector record"
}

// documentOrigin names the folder a copied file came from, when the ingest
// tagged one. The folder name is a hint about the writer, never a verdict.
func documentOrigin(m memory.Memory) string {
	for _, tag := range m.Tags {
		if tag != "" && !strings.EqualFold(tag, "filesystem") {
			return " (source " + quoteLabel(tag) + ")"
		}
	}
	return ""
}

func sourceSuffix(m memory.Memory) string {
	label := strings.TrimSpace(m.Source)
	if label == "" {
		return ""
	}
	// For the agent-note mirrors the label is a long staging path. The reader
	// needs to know which note it is, not where the ingest staged it, and the
	// whole path both wastes the budget and repeats a directory layout on
	// every row. The last two segments are kept, so two notes of the same name
	// under different projects still read differently, and the full path is
	// still on the record for anyone who opens it. A label is only treated as
	// a path when it is absolute, has no spaces and ends in a file extension,
	// so a label like /review/approved is left exactly as the writer typed it.
	if strings.HasPrefix(label, "/") && !strings.ContainsFunc(label, unicode.IsSpace) && path.Ext(label) != "" {
		if parent, file := path.Split(label); file != "" {
			label = path.Join(path.Base(strings.TrimSuffix(parent, "/")), file)
		}
	}
	return " (source " + quoteLabel(label) + ")"
}

// quoteLabel collapses a free-text label to one short quoted phrase so a
// rambling source line cannot swallow the budget.
func quoteLabel(label string) string {
	clean := strings.Join(strings.Fields(label), " ")
	clean = strings.ReplaceAll(clean, `"`, "'")
	if utf8.RuneCountInString(clean) > sourceLabelMaxRunes {
		clean = string([]rune(clean)[:sourceLabelMaxRunes]) + "…"
	}
	return `"` + clean + `"`
}

// adoptionLine reports what the vault records about the user adopting a
// decision, which today is nothing. No field anywhere says a human accepted a
// decision, so the line never says one did, and it says the same thing for
// every decision. A source label claiming the user confirmed something is the
// writer's wording; it is shown on the provenance line and never promoted here.
func adoptionLine(m memory.Memory) string {
	if m.Decision == nil && !strings.EqualFold(m.Type, "decision") {
		return ""
	}
	return "Adopted by user: no record either way"
}

// laterRelatedLine points at a newer record about the same subject. It is a
// reason to read both, never a claim that the older one is dead.
func laterRelatedLine(m memory.Memory) string {
	later := m.LaterRelatedEvidence
	if later == nil {
		return ""
	}
	return fmt.Sprintf("Later related record: %s %q (%s). Read it before relying on this one.", later.ID, later.Title, later.IndexedAt)
}

// BudgetResults keeps the largest whole-record prefix under a conservative JSON byte budget.
func BudgetResults(mems []memory.Memory, budgetBytes int) (kept []memory.Memory, dropped int) {
	if budgetBytes <= 0 || len(mems) == 0 {
		return mems, 0
	}
	const jsonSep = 2
	kept = make([]memory.Memory, 0, len(mems))
	used := 0
	for _, m := range mems {
		body, err := json.Marshal(m)
		cost := jsonSep
		if err == nil {
			cost += len(body)
		}
		if used+cost > budgetBytes && len(kept) > 0 {
			break
		}
		kept = append(kept, m)
		used += cost
	}
	return kept, len(mems) - len(kept)
}

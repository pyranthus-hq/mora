package mora

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	indexstore "github.com/pyranthus-hq/mora/internal/index"
	"github.com/pyranthus-hq/mora/internal/memory"
)

const (
	desktopReviewTitleBytes     = 200
	desktopReviewTextBytes      = 20000
	desktopReviewFieldBytes     = 512
	desktopReviewDetailCount    = 16
	maxDesktopReviewMemoryBytes = maxProposalInspectionBytes
)

type desktopReviewField struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type desktopReviewDisplay struct {
	ID               string               `json:"id"`
	Kind             string               `json:"kind"`
	Title            string               `json:"title"`
	Text             string               `json:"text"`
	Scope            string               `json:"scope"`
	Type             string               `json:"type"`
	Source           string               `json:"source"`
	Provenance       string               `json:"provenance"`
	State            string               `json:"state"`
	Details          []desktopReviewField `json:"details,omitempty"`
	Complete         bool                 `json:"complete"`
	Redacted         bool                 `json:"redacted"`
	Truncated        bool                 `json:"truncated"`
	Escaped          bool                 `json:"escaped"`
	OmittedArguments bool                 `json:"omitted_arguments"`
}

// desktopReviewPreview carries the revision checked by the terminal review
// guards: saved-file SHA for confirmMemory and undoMemory, and the canonical
// proposal-argument digest for decideMCPWriteProposalVia. DisplayDigest binds
// the exact safe display separately. Neither digest grants approval.
type desktopReviewPreview struct {
	desktopReviewDisplay
	CanonicalRevision string `json:"canonical_revision"`
	DisplayDigest     string `json:"display_digest"`
}

var errDesktopReviewUnavailable = errors.New("review item is unavailable or no longer pending")

func loadDesktopReviewPreview(cfg Config, kind, id string) (desktopReviewPreview, error) {
	var note reviewNote
	var err error
	var state, revision string
	var redacted, omittedArguments bool
	switch kind {
	case reviewKindSaved:
		note, err = loadDesktopSavedReviewNote(cfg, id)
		state, revision = "tentative", note.SHA
	case reviewKindHeld:
		proposal, _, readErr := readMCPProposalForInspection(cfg, id)
		if readErr != nil {
			return desktopReviewPreview{}, errDesktopReviewUnavailable
		}
		decision, readErr := readMCPProposalDecision(cfg, id)
		if readErr != nil || decision != nil {
			return desktopReviewPreview{}, errDesktopReviewUnavailable
		}
		note, err = reviewNoteFromProposal(proposal)
		state, revision = "pending", note.Digest
		_, redacted = redactProposalValue(proposal.Arguments)
		omittedArguments = !desktopReviewShowsArguments(proposal.Arguments, note)
	default:
		return desktopReviewPreview{}, errDesktopReviewUnavailable
	}
	if err != nil || note.ID != id || note.Deleted || revision == "" {
		return desktopReviewPreview{}, errDesktopReviewUnavailable
	}

	display := desktopReviewDisplay{
		Kind: kind, State: state, Provenance: note.Provenance,
		Redacted: redacted, Truncated: note.Truncated, OmittedArguments: omittedArguments,
	}
	display.ID = display.safeField(note.ID, desktopReviewFieldBytes, false)
	display.Title = display.safeField(note.Title, desktopReviewTitleBytes, false)
	display.Text = display.safeField(note.Text, desktopReviewTextBytes, true)
	display.Scope = display.safeField(note.Scope, desktopReviewFieldBytes, false)
	display.Type = display.safeField(note.Type, desktopReviewFieldBytes, false)
	display.Source = display.safeField(note.Source, desktopReviewFieldBytes, false)
	for _, field := range note.Fields {
		switch field[0] {
		case "scope", "type", "title", "text", "source":
			continue
		case "meta":
			// The shared recursive inspector also recognizes sensitive field names.
			if _, hidden := redactProposalValue(note.Meta); hidden {
				display.Redacted = true
				field[1] = "[REDACTED]"
			}
		}
		display.addDetail(field[0], field[1])
	}
	if len(note.Reasons) != 0 {
		display.addDetail("held because", strings.Join(note.Reasons, ", "))
	}
	display.Complete = !display.Redacted && !display.Truncated && !display.Escaped && !display.OmittedArguments

	return newDesktopReviewPreview(display, revision)
}

func newDesktopReviewPreview(display desktopReviewDisplay, revision string) (desktopReviewPreview, error) {
	raw, err := json.Marshal(display)
	if err != nil {
		return desktopReviewPreview{}, err
	}
	digest := indexstore.ContentSHA256(append([]byte("mora-desktop-review-display-v1\n"), raw...))
	return desktopReviewPreview{desktopReviewDisplay: display, CanonicalRevision: revision, DisplayDigest: digest}, nil
}

// Use the same ID lookup and visibility policy as terminal review. Hash and
// parse the same bounded snapshot instead of opening the chosen path again.
func loadDesktopSavedReviewNote(cfg Config, id string) (reviewNote, error) {
	root, err := os.OpenRoot(cfg.VaultDir)
	if err != nil {
		return reviewNote{}, err
	}
	defer root.Close()
	var revision string
	record, err := findMemoryRawWithReader(cfg, id, func(path string) (Memory, error) {
		relative, err := filepath.Rel(cfg.VaultDir, path)
		if err != nil {
			return Memory{}, err
		}
		info, err := root.Lstat(relative)
		if err != nil {
			return Memory{}, err
		}
		if !info.Mode().IsRegular() || info.Size() > maxDesktopReviewMemoryBytes {
			return Memory{}, errDesktopReviewUnavailable
		}
		file, err := root.Open(relative)
		if err != nil {
			return Memory{}, err
		}
		defer file.Close()
		raw, err := readReviewSnapshot(file, info, maxDesktopReviewMemoryBytes)
		if err != nil {
			return Memory{}, err
		}
		m, err := parseMemoryBytes(path, raw)
		if err == nil && m.ID == id {
			revision = indexstore.ContentSHA256(raw)
		}
		return m, err
	})
	if err != nil || !memory.IsTentative(record) {
		return reviewNote{}, errDesktopReviewUnavailable
	}
	if err := requireCurrentMemory(cfg, id); err != nil {
		return reviewNote{}, err
	}
	note := reviewNoteFromMemory(record)
	note.SHA = revision
	return note, nil
}

// Only explicitly rendered values can make a complete proposal preview.
// A future argument that the app cannot display must stay incomplete even
// if today's write implementation ignores it.
func desktopReviewShowsArguments(args map[string]any, note reviewNote) bool {
	for key, raw := range args {
		value, ok := raw.(string)
		if !ok {
			return false
		}
		switch key {
		case "title":
			ok = value == note.Title
		case "text":
			ok = value == note.Text
		case "scope":
			ok = value == note.Scope
		case "type":
			ok = value == note.Type
		case "source":
			ok = value == note.Source
		case "target", "disposition":
			ok = note.Meta[key] == value
		case "as_of":
			ok = desktopReviewHasField(note, "decision as of", value)
		case "durability":
			ok = desktopReviewHasField(note, "decision durability", value)
		case "review_by":
			ok = desktopReviewHasField(note, "decision review by", value)
		case "flip_conditions":
			var conditions []string
			for _, field := range note.Fields {
				if field[0] == "decision flip condition" {
					conditions = append(conditions, field[1])
				}
			}
			ok = len(conditions) != 0 && strings.Join(conditions, ";") == value
		default:
			return false
		}
		if !ok {
			return false
		}
	}
	return true
}

func desktopReviewHasField(note reviewNote, label, value string) bool {
	for _, field := range note.Fields {
		if field[0] == label {
			return field[1] == value
		}
	}
	return false
}

func (d *desktopReviewDisplay) addDetail(label, value string) {
	if len(d.Details) == desktopReviewDetailCount {
		d.Truncated = true
		return
	}
	d.Details = append(d.Details, desktopReviewField{
		Label: label,
		Value: d.safeField(value, desktopReviewFieldBytes, false),
	})
}

func (d *desktopReviewDisplay) safeField(value string, limit int, multiline bool) string {
	value, hidden := redactProposalText(value)
	d.Redacted = d.Redacted || hidden
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "\ufffd")
		d.Escaped = true
	}
	value = d.limitField(value, limit)
	if strings.IndexFunc(value, func(r rune) bool { return !unicode.IsGraphic(r) && (!multiline || r != '\n') }) >= 0 {
		if multiline {
			lines := strings.Split(value, "\n")
			for i := range lines {
				lines[i] = rulesShow(lines[i])
			}
			value = strings.Join(lines, "\n")
		} else {
			value = rulesShow(value)
		}
		d.Escaped = true
	}
	return d.limitField(value, limit)
}

func (d *desktopReviewDisplay) limitField(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	d.Truncated = true
	for !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

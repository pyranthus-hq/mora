package exam

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	emailToken = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@([a-z0-9-]+(?:\.[a-z0-9-]+)+)`)
	urlDomain  = regexp.MustCompile(`(?i)\bhttps?://([a-z0-9-]+(?:\.[a-z0-9-]+)+)`)
	plusPhone  = regexp.MustCompile(`\+[0-9]{11,15}\b`)
	parenPhone = regexp.MustCompile(`\([0-9]{3}\)[ ]*[0-9]{3}-[0-9]{4}`)
	dashPhone  = regexp.MustCompile(`\b[0-9]{3}-[0-9]{3}-[0-9]{4}\b`)
	barePhone  = regexp.MustCompile(`\b[0-9]{10}\b`)
	digitsOnly = regexp.MustCompile(`[0-9]`)
)

func Lint(l Ledger) error {
	b, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("%s: encode ledger: %w", LintRealIdentity, err)
	}
	return lintBytes(LintRealIdentity, "ledger", b)
}

func LintCorpus(files map[string][]byte) error {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		b := files[path]
		if err := lintBytes(LintCorpusBytes, path, b); err != nil {
			return err
		}
	}
	return nil
}

// labelLeakVocab are meta terms from the exam's own label language (verdict,
// lifecycle, and non-obligation class names). They describe how a record is
// SCORED, not what a real person would write, so their presence in an
// auditor-visible field hands the blinded auditor the answer. Kept to
// unambiguous jargon so benign in-world prose does not trip it.
var labelLeakVocab = []string{
	"superseded", "bystander", "non-obligation", "false positive",
	"true positive", "gold miss", "gold_miss", "third-party attendee",
	"duplicate copy", "test purpose", "verdict",
}

// LintLeakage rejects a ledger whose auditor-visible fields (artifact subjects
// and rendered block text) restate the gold label. It catches the two channels
// that invalidated the obligations-v1 human sitting: a subject byte-equal to a
// commitment summary, and body prose that narrates its own verdict. The
// transition-evidence quote is intentionally NOT checked against the body it
// cites — that span equality is required by design, not a leak.
func LintLeakage(l Ledger) error {
	summaries := map[string]string{} // normalized summary -> commitment id
	for _, c := range l.Commitments {
		if s := normalizeLeak(c.Summary); s != "" {
			summaries[s] = c.ID
		}
	}
	whys := make([]string, 0, len(l.NonObligations))
	for _, n := range l.NonObligations {
		if w := normalizeLeak(n.Why); w != "" {
			whys = append(whys, w)
		}
	}
	for _, a := range l.Artifacts {
		visible := []struct{ field, text string }{{"subject", a.Subject}}
		for _, m := range a.Messages {
			for _, b := range m.Body {
				visible = append(visible, struct{ field, text string }{"block " + b.ID, b.Text})
			}
		}
		for _, v := range visible {
			norm := normalizeLeak(v.text)
			if norm == "" {
				continue
			}
			if cid, ok := summaries[norm]; ok {
				return fmt.Errorf("ERR_%s [%s]: artifact %q %s restates commitment %q gold summary verbatim", strings.ToUpper(LintLabelLeak), LintLabelLeak, a.ID, v.field, cid)
			}
			for _, term := range labelLeakVocab {
				if strings.Contains(norm, term) {
					return fmt.Errorf("ERR_%s [%s]: artifact %q %s contains label-language %q", strings.ToUpper(LintLabelLeak), LintLabelLeak, a.ID, v.field, term)
				}
			}
			for _, w := range whys {
				if strings.Contains(norm, w) {
					return fmt.Errorf("ERR_%s [%s]: artifact %q %s restates a non-obligation rationale", strings.ToUpper(LintLabelLeak), LintLabelLeak, a.ID, v.field)
				}
			}
		}
	}
	return nil
}

// leakClasses splits artifacts into the classes a blinded reader is asked to
// separate: positives open at least one canonical still-open commitment,
// negatives are everything else. Both fingerprint gates below ask the same
// question about a different metadata channel: can this channel ALONE predict
// the class? If it can, the corpus grades pattern-matching, not reading — the
// exact defect that invalidated the obligations-v1 human sitting.
func leakClasses(l Ledger) (pos, neg []Artifact) {
	positive := map[string]bool{}
	for _, c := range l.Commitments {
		if c.State == "open" && c.DuplicateOf == "" {
			positive[c.OpenedBy.ArtifactID] = true
		}
	}
	for _, a := range l.Artifacts {
		if positive[a.ID] {
			pos = append(pos, a)
		} else {
			neg = append(neg, a)
		}
	}
	return pos, neg
}

// LintDateFingerprint rejects a ledger whose artifact dates separate the
// classes: if every positive is newer (or older) than every negative, the date
// column is the answer key. Ranges must overlap.
func LintDateFingerprint(l Ledger) error {
	pos, neg := leakClasses(l)
	if len(pos) == 0 || len(neg) == 0 {
		return nil
	}
	// Full-timestamp granularity on purpose: all-positives-before-noon on a
	// shared day is as much a fingerprint as a disjoint week. RFC3339 with a
	// fixed offset compares lexically; the validator enforces the format, and
	// a malformed timestamp here fails loud rather than sliding by.
	bounds := func(as []Artifact) (min, max string, err error) {
		for i, a := range as {
			at := a.OccurredAt
			if _, perr := time.Parse(time.RFC3339, at); perr != nil {
				return "", "", fmt.Errorf("ERR_%s [%s]: artifact %q occurred_at %q is not RFC3339", strings.ToUpper(LintDateLeak), LintDateLeak, a.ID, at)
			}
			if i == 0 || at < min {
				min = at
			}
			if i == 0 || at > max {
				max = at
			}
		}
		return min, max, nil
	}
	posMin, posMax, err := bounds(pos)
	if err != nil {
		return err
	}
	negMin, negMax, err := bounds(neg)
	if err != nil {
		return err
	}
	if posMin > negMax || negMin > posMax {
		return fmt.Errorf("ERR_%s [%s]: artifact dates alone separate the classes (positives %s..%s, negatives %s..%s)", strings.ToUpper(LintDateLeak), LintDateLeak, posMin, posMax, negMin, negMax)
	}
	return nil
}

// LintTitleFingerprint rejects a ledger where a single subject token acts as a
// class label: present in every positive subject and no negative subject (or
// the reverse). Needs at least two artifacts per class to mean anything.
func LintTitleFingerprint(l Ledger) error {
	pos, neg := leakClasses(l)
	if len(pos) < 2 || len(neg) < 2 {
		return nil
	}
	tokens := func(as []Artifact) []map[string]bool {
		out := make([]map[string]bool, 0, len(as))
		for _, a := range as {
			set := map[string]bool{}
			for _, t := range titleToken.FindAllString(strings.ToLower(a.Subject), -1) {
				// Two-rune floor: even "due" or "ask" as a perfect class
				// marker is a leak; only single characters are too noisy to
				// mean anything.
				if len([]rune(t)) >= 2 {
					set[t] = true
				}
			}
			out = append(out, set)
		}
		return out
	}
	separates := func(covered, empty []map[string]bool) string {
		for t := range covered[0] {
			all := true
			for _, set := range covered[1:] {
				all = all && set[t]
			}
			if !all {
				continue
			}
			none := true
			for _, set := range empty {
				none = none && !set[t]
			}
			if none {
				return t
			}
		}
		return ""
	}
	posTokens, negTokens := tokens(pos), tokens(neg)
	if t := separates(posTokens, negTokens); t != "" {
		return fmt.Errorf("ERR_%s [%s]: subject token %q marks every positive and no negative", strings.ToUpper(LintTitleLeak), LintTitleLeak, t)
	}
	if t := separates(negTokens, posTokens); t != "" {
		return fmt.Errorf("ERR_%s [%s]: subject token %q marks every negative and no positive", strings.ToUpper(LintTitleLeak), LintTitleLeak, t)
	}
	return nil
}

var titleToken = regexp.MustCompile(`[\p{L}\p{N}]+`)

var leakSpace = regexp.MustCompile(`\s+`)

func normalizeLeak(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = leakSpace.ReplaceAllString(s, " ")
	return strings.Trim(s, " .;:,!?")
}

func lintBytes(rule, path string, b []byte) error {
	s := strings.ToLower(string(b))
	for _, forbidden := range []string{"karode", "neil patel", "pyranthus", "halcyon", "northwind"} {
		if strings.Contains(s, forbidden) {
			return fmt.Errorf("ERR_%s [%s]: %s contains forbidden identity literal %q", strings.ToUpper(rule), rule, path, forbidden)
		}
	}
	for _, match := range emailToken.FindAllStringSubmatch(s, -1) {
		if !reservedDomain(match[1]) {
			return fmt.Errorf("ERR_%s [%s]: %s contains non-reserved email %q", strings.ToUpper(rule), rule, path, match[0])
		}
	}
	for _, match := range urlDomain.FindAllStringSubmatch(s, -1) {
		if !reservedDomain(match[1]) {
			return fmt.Errorf("ERR_%s [%s]: %s contains non-reserved URL domain %q", strings.ToUpper(rule), rule, path, match[1])
		}
	}
	for _, token := range plusPhone.FindAllString(s, -1) {
		if !fictionalHandle(token) {
			return fmt.Errorf("ERR_%s [%s]: %s contains non-fictional handle %q", strings.ToUpper(rule), rule, path, token)
		}
	}
	for _, re := range []*regexp.Regexp{parenPhone, dashPhone, barePhone} {
		for _, token := range re.FindAllString(s, -1) {
			digits := digitsOnly.FindAllString(token, -1)
			if len(digits) != 10 {
				return fmt.Errorf("ERR_%s [%s]: %s contains malformed handle %q", strings.ToUpper(rule), rule, path, token)
			}
			joined := strings.Join(digits, "")
			line, _ := strconv.Atoi(joined[6:])
			if joined[:6] != "555010" || line < 100 || line > 199 {
				return fmt.Errorf("ERR_%s [%s]: %s contains non-fictional handle %q", strings.ToUpper(rule), rule, path, token)
			}
		}
	}
	return nil
}

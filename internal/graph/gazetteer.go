package graph

import (
	"sort"
	"strings"
	"unicode"

	embedpkg "github.com/pyranthus-hq/mora/internal/embed"
)

// Gazetteer body-matching finds known people mentioned in free text (message and
// email bodies) without NER or a model: the Gazetteer is built from the graph's
// own person aliases, and matched against body tokens on word boundaries. It is
// deliberately HIGH PRECISION over recall — a wrong MENTIONS edge pollutes the
// graph, so the guards below err toward missing a mention rather than inventing one.

func tokenizeForScan(s string) ([]string, []bool) { return embedpkg.TokenizeForScan(s) }
func tokenizeWords(s string) []string             { return embedpkg.TokenizeWords(s) }

const (
	// minGazNameLen is the shortest display name eligible for body matching.
	minGazNameLen = 5
	// maxGazWindow is the longest multi-token name the scanner will match.
	maxGazWindow = 4
)

// gazStoplist holds generic, non-personal display-name tokens (mailing lists,
// automated senders). A name containing any of these — or any hyphen sub-part of a
// token (so "support-team" is screened too) — is excluded: "Support Team" or "No
// Reply" must never match a person in a body.
var gazStoplist = map[string]bool{
	"no": true, "reply": true, "noreply": true, "no-reply": true, "donotreply": true,
	"do-not-reply": true, "info": true, "support": true, "team": true, "admin": true,
	"notifications": true, "notification": true, "newsletter": true, "alerts": true,
	"alert": true, "account": true, "accounts": true, "mailer": true, "daemon": true,
	"help": true, "sales": true, "billing": true, "service": true, "services": true,
	"updates": true, "update": true, "noreply-": true, "bot": true, "automated": true,
	"push": true, "author": true, "mention": true, "ci": true, "activity": true, "state": true, "change": true,
}

// gazWordStoplist holds common English function words that frequently sit adjacent
// to another word in prose. A display name whose tokens are these would match
// ordinary sentences ("the leaves will brown", "see you may day"), so they are
// excluded — such a person is still captured precisely via metadata. Pure
// grammatical words only; real surnames/given names (Brown, Long, Mark) are NOT
// listed, to preserve recall.
var gazWordStoplist = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"for": true, "with": true, "from": true, "to": true, "of": true, "in": true,
	"on": true, "at": true, "by": true, "will": true, "may": true, "can": true,
	"shall": true, "should": true, "would": true, "could": true, "are": true,
	"was": true, "were": true, "is": true, "be": true, "been": true, "have": true,
	"has": true, "had": true, "do": true, "does": true, "did": true, "this": true,
	"that": true, "these": true, "those": true, "you": true, "your": true,
	"our": true, "their": true, "not": true, "no": true, "yes": true,
	// Question words — never a personal-name token; keeps a title-cased question
	// phrase ("Where Tahoe", "What Did") from being mistaken for a name (codex I3).
	"what": true, "when": true, "where": true, "why": true, "who": true,
	"how": true, "which": true, "whose": true, "whom": true,
}

// Gazetteer maps a normalized multi-token name to the canonical person id it
// resolves to (ambiguous names already tie-broken deterministically).
type Gazetteer map[string]string

// buildGazetteer derives the Gazetteer from the person aggregates. Only multi-token
// display names survive (single first names are too ambiguous to match in free text
// without NER — deferred). On an ambiguous name shared by two people, the one with
// more metadata evidence wins, then the smaller id (deterministic tie-break).
func buildGazetteer(persons map[string]*personAgg) Gazetteer {
	type cand struct {
		id    string
		score int
	}
	byName := map[string][]cand{}
	// Deterministic iteration: sort person ids.
	ids := make([]string, 0, len(persons))
	for id := range persons {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := persons[id]
		for _, alias := range sortedKeys(p.aliases) {
			norm, ok := normalizeGazName(alias)
			if !ok {
				continue
			}
			byName[norm] = append(byName[norm], cand{id: id, score: len(p.evidence)})
		}
	}
	g := Gazetteer{}
	for norm, cands := range byName {
		best := cands[0]
		for _, c := range cands[1:] {
			if c.score > best.score || (c.score == best.score && c.id < best.id) {
				best = c
			}
		}
		g[norm] = best.id
	}
	return g
}

// normalizeGazName lowercases and tokenizes a display name, returning the
// space-joined normalized form and whether it is eligible (a real, multi-token
// person name: ≥2 tokens, ≥minGazNameLen chars, not an email/handle, no stoplist
// token).
func normalizeGazName(name string) (string, bool) {
	if strings.ContainsAny(name, "@+") { // an email or phone handle, not a display name
		return "", false
	}
	toks := tokenizeWords(name)
	if len(toks) < 2 {
		return "", false
	}
	hasLong := false
	for _, t := range toks {
		// Stoplist screens the whole token AND its hyphen sub-parts, so a hyphenated
		// generic ("support-team") is caught (codex: hyphen bypass).
		if gazStoplist[t] || gazWordStoplist[t] {
			return "", false
		}
		for _, part := range strings.Split(t, "-") {
			if gazStoplist[part] {
				return "", false
			}
		}
		if !hasLetter(t) {
			return "", false
		}
		rc := len([]rune(t))
		if rc < 2 { // single-rune token = an initial ("A Smith") — too ambiguous
			return "", false
		}
		if rc >= 3 {
			hasLong = true
		}
	}
	if !hasLong { // need at least one substantive (≥3-rune) token
		return "", false
	}
	norm := strings.Join(toks, " ")
	if len(norm) < minGazNameLen {
		return "", false
	}
	return norm, true
}

// gazetteerScan returns the canonical person ids whose names appear in text, each
// at most once, sorted. Longer names are matched greedily (so "neil patel" wins
// over a hypothetical "neil") and the scan advances past a match — no overlapping
// double counts.
func gazetteerScan(g Gazetteer, text string) []string {
	if len(g) == 0 {
		return nil
	}
	toks, joinable := tokenizeForScan(text)
	found := map[string]bool{}
	for i := 0; i < len(toks); {
		matched := 0
		for w := maxGazWindow; w >= 2; w-- {
			if i+w > len(toks) {
				continue
			}
			// A multi-token name only matches when every INTERNAL gap was plain
			// space — never punctuation/newline. Otherwise "john.doe@example.com"
			// (tokens john,doe,…) would falsely match the person "John Doe", and
			// "John.\nSmith" would too (codex P1).
			ok := true
			for j := i + 1; j < i+w; j++ {
				if !joinable[j] {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			if id, hit := g[strings.Join(toks[i:i+w], " ")]; hit {
				found[id] = true
				matched = w
				break
			}
		}
		if matched == 0 {
			i++
		} else {
			i += matched
		}
	}
	if len(found) == 0 {
		return nil
	}
	out := make([]string, 0, len(found))
	for id := range found {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// tokenizeWords splits text into lowercased word tokens — maximal runs of letters,
// digits, apostrophes, and hyphens. Splitting on every other rune gives word-
// boundary matching for free ("Samuelson" never matches "Sam").

// tokenizeForScan tokenizes like tokenizeWords but also reports, per token,
// whether the separator immediately before it was made up of ONLY ASCII space/tab
// (joinable[i]). A name match may bridge tokens i..i+w-1 only when every internal
// joinable flag is true, so punctuation, '@', '/', and newlines all block a match.
// joinable[0] is false (no preceding gap).

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

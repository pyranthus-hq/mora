package mora

import (
	"regexp"
	"strings"
	"unicode"
)

// holdPattern is one reason a write waits for the owner and the test that
// decides it. match gets raw text and normalises it itself, so every caller
// (the write gate and the handoff capture) sees the same text.
type holdPattern struct {
	reason string
	match  func(string) bool
}

// tentativeHoldPatterns name text an agent may not save without review. The
// list is a coarse filter, not a security boundary: a false claim in plain
// words passes it. Tentative rows are safe because they stay out of
// authoritative surfaces, never because this filter caught everything.
var tentativeHoldPatterns = []holdPattern{
	{"sensitive", func(s string) bool { return sensitivePattern.MatchString(normalizeHoldText(s)) }},
	{"aimed at agents", agentAimedMatch},
}

var sensitivePattern = regexp.MustCompile(`(?i)\b(ssn|social security|passport|medical|diagnos(is|ed)|lawsuit|attorney|immigration|visa status)\b`)

// holdPrincipal is the singular person an agent answers to, with an article or
// possessive: "the user", "your owner". Plural "users" and a bare "user input"
// are not principals. holdPrincipalEnd keeps "the user table" from counting.
const (
	holdPrincipalNoun = `(?:the|your|this|that|my)\s+(?:user|owner|human|operator)`
	holdPrincipalEnd  = `(?:\s+(?:about|of|that|who|when|if|before|after|what|how|why|whether|know|knows|knowing|noticing|seeing|to|for|on|in|at|with|and|or|but|the|a|an|any|his|her|their|its|this|it|them|is|are|was|were|has|have|should|must|will|can|may|might|would|could)\b|\s*[.,;:!?)"']|\s*$)`
	holdPrincipal     = holdPrincipalNoun + holdPrincipalEnd
	holdClause        = `[^.!?;]`
)

// agentAimedPatterns match text that talks to an agent instead of recording a
// fact, grouped by what is being subverted: (a) approval and oversight, (b)
// concealment from the person the agent answers to, (c) instruction override,
// (d) download-to-interpreter. They run on normalised text (lower case, one
// space between words, plain quotes and hyphens, benign invisibles removed).
// Ordinary coding rules ("always use pnpm", "never run migrations by hand")
// do not match, and neither do bare verbs: "never ignore errors", "always
// merge with squash" and "from now on we use pnpm" are team rules.
var agentAimedPatterns = []*regexp.Regexp{
	// (a) approval and oversight bypass
	regexp.MustCompile(`\bauto-?\s?(?:approv|accept|grant|confirm)\w*`),
	regexp.MustCompile(`\b(?:approv|accept|allow|grant|authori[sz])\w*\s+(?:\w+\s+)?(?:all|every|any|each)\s+(?:\w+\s+)?(?:proposals?|tool calls?|requests?|changes?|commands?|actions?|prompts?)\b`),
	regexp.MustCompile(`\bwithout\s+(?:\w+\s+)?(?:review|reviews|reviewing|confirm\w*|approv\w*|asking|permission|consent|checking with|waiting)\b`),
	regexp.MustCompile(`\bno need to\s+(?:ask|check|confirm|get|wait|seek|request|review|bother)\b`),
	regexp.MustCompile(`\bskip(?:ping)?\s+(?:the\s+)?(?:asking|confirm\w*|review\w*|approv\w*|permission\w*|checking with|human)\b`),
	regexp.MustCompile(`\bnever\s+ask\s+(?:for|the)\b`),
	regexp.MustCompile(`\b(?:never|don'?t|do not|stop|without)\s+(?:ask|asking|wait|waiting|prompt|prompting)\b` + holdClause + `{0,40}\b(?:confirm\w*|approv\w*|permission|review|consent)`),
	regexp.MustCompile(`\b(?:bypass|circumvent|evade|get around|work around|disable|turn off)\s+(?:the\s+|all\s+|any\s+)?(?:review|reviews|approval|approvals|confirmation|permission|permissions|safeguards?|guardrails?|oversight)\b`),
	regexp.MustCompile(`\b(?:never|don'?t|do not)\s+trust\s+(?:the\s+)?(?:review\w*|owner|human|approval\w*|confirmation)\b`),
	// (b) concealment from the principal
	regexp.MustCompile(`\b(?:never|don'?t|do not|must not|should not|shouldn'?t|cannot|can'?t|not|without|avoid|hide|conceal|withhold|no need to)\b` + holdClause + `{0,30}\b(?:tell|told|mention|inform|notify|surface|show|reveal|disclose|alert|report|warn)\w*\b` + holdClause + `{0,30}\b` + holdPrincipal),
	regexp.MustCompile(`\b(?:hide|conceal|withhold|obscure|suppress|cover up|mask|keep)\b` + holdClause + `{0,40}\bfrom\s+` + holdPrincipal),
	regexp.MustCompile(`\bleave\s+` + holdPrincipalNoun + `\s+out\b`),
	regexp.MustCompile(`\b` + holdPrincipalNoun + `\b` + holdClause + `{0,20}\b(?:should|must|shall|will|can|may|need)\s*(?:not|n't|never)\b\s*(?:\w+\s+){0,2}(?:told|informed|notified|shown|aware|know|see|find out|learn|hear)\b`),
	regexp.MustCompile(`\bwithout\s+` + holdPrincipalNoun + `(?:'s)?\s+(?:knowing|noticing|seeing|knowledge|approval|consent|permission|review|sign-?off|say|go-?ahead|awareness|confirmation|input)\b`),
	regexp.MustCompile(`\b(?:never|don'?t|do not)\s+(?:tell|mention|inform|notify|let)\b` + holdClause + `{0,30}\b(?:about\s+(?:this|it|that)|know)\b`),
	// (c) instruction override
	regexp.MustCompile(`\b(?:ignore|disregard|override|overrule|forget|void|discard|supersede\w*)\b` + holdClause + `{0,30}?\b(?:previous|prior|earlier|above|preceding|former|original|initial|system)\s+(?:\w+\s+){0,2}(?:instructions?|guidance|guidelines|rules|prompts?|directives?|constraints|restrictions)\b`),
	regexp.MustCompile(`\b(?:ignore|disregard|override|overrule|forget|void|discard|supersede\w*)\b` + holdClause + `{0,30}?\b(?:all|any|every|your|these|those|my)\s+(?:\w+\s+)?(?:instructions?|guidance|directives?)\b`),
	regexp.MustCompile(`\b(?:previous|prior|earlier|above|preceding|former|original|all)\s+(?:\w+\s+){0,2}(?:instructions?|guidance|rules|prompts?|directives?)\b` + holdClause + `{0,25}\b(?:void|invalid|obsolete|null|moot|cancell?ed|revoked|superseded|overridden|ignored|disregarded|no longer (?:apply|valid|matter))\b`),
	regexp.MustCompile(`\b(?:ignore|disregard|override|overrule|forget|void|bypass|discard)\b` + holdClause + `{0,30}\b(?:instructions?|guidance|rules|prompts?)\b` + holdClause + `{0,15}\b(?:claude|agents|gemini|cursor|copilot)\.mdc?\b`),
	regexp.MustCompile(`\b(?:higher priority|precedence|priority)\s+(?:than|over)\s+(?:your|the|any|all|those|these)\s+(?:\w+\s+){0,2}(?:instructions?|guidance|directives?|prompts?)\b`),
	regexp.MustCompile(`\b(?:print|show|reveal|output|dump|leak|repeat|display|reproduce|share|expose|echo|recite|disclose)\b` + holdClause + `{0,20}\b(?:your|the|its|this)\s+(?:system|initial|hidden|original)\s+(?:prompt|instructions?|message)\b`),
	regexp.MustCompile(`\b(?:agents?|assistants?|claude|codex)\s+(?:must|should|shall|need to|have to)\b`),
	regexp.MustCompile(`\bfrom now on\b` + holdClause + `{0,20}\b(?:skip|ignore|stop|bypass|approve|accept|disregard|override|disable|auto-?approve|trust|obey)\w*`),
	// (d) download straight into an interpreter
	regexp.MustCompile(`\b(?:curl|wget)\b.{0,300}\|\s*(?:sudo\b\s*(?:-\S+\s+)*)?(?:env\s+(?:\S+=\S*\s+)*)?(?:sudo\s+(?:-\S+\s+)*)?(?:sh|bash|zsh|dash|ksh|python[\d.]*|node|nodejs|perl|ruby|php|pwsh|powershell|iex)\b`),
	regexp.MustCompile(`\b(?:sh|bash|zsh|dash|ksh)\s+(?:-\w+\s+)*<\(\s*(?:curl|wget)\b`),
	regexp.MustCompile(`\b(?:source|eval)\s+["']?[<$]\(\s*(?:curl|wget)\b`),
	regexp.MustCompile(`\b(?:sh|bash|zsh)\s+-c\s+["']?\$\(\s*(?:curl|wget)\b`),
}

var (
	holdForgetPattern = regexp.MustCompile(`\b(?:don'?t|do not|never)\s+forget\s+to\b`)
	holdJSONToolOK    = regexp.MustCompile(`python[\d.]*\s+-m\s+json\.tool`)
)

// agentAimedMatch reports whether text talks to an agent. See agentAimedPatterns.
func agentAimedMatch(s string) bool {
	n := normalizeHoldText(s)
	// "Don't forget to tell the user" is a reminder, not a concealment.
	n = holdForgetPattern.ReplaceAllString(n, "always remember to")
	// Pretty-printing JSON is the one common pipe into an interpreter.
	n = holdJSONToolOK.ReplaceAllString(n, "jsontool")
	for _, p := range agentAimedPatterns {
		if p.MatchString(n) {
			return true
		}
	}
	return false
}

// normalizeHoldText prepares text for the hold patterns: lower case, curly
// quotes and unicode dashes made plain, every control character and run of
// whitespace one space, and benign invisible characters removed so they cannot
// split a trigger word.
func normalizeHoldText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case isBenignInvisible(r):
		case r == 0x2018 || r == 0x2019 || r == 0x201B || r == 0x02BC || r == 0x2032 || r == 0x00B4:
			b.WriteByte('\'')
		case r == 0x201C || r == 0x201D || r == 0x201E || r == 0x201F || r == 0x00AB || r == 0x00BB:
			b.WriteByte('"')
		case (r >= 0x2010 && r <= 0x2015) || r == 0x2212 || r == 0xFE58 || r == 0xFF0D:
			b.WriteByte('-')
		case unicode.IsSpace(r) || unicode.IsControl(r) || r == 0x2028 || r == 0x2029:
			b.WriteByte(' ')
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// isBenignInvisible reports an invisible character that ordinary text uses:
// variation selectors 15 and 16 (emoji presentation), the zero width joiner
// (joined emoji), the zero width non-joiner (Persian and Indic scripts), the
// left and right marks, the byte order mark, the soft hyphen, and BMP private
// use (icon fonts). None is a hold reason by itself.
func isBenignInvisible(r rune) bool {
	switch r {
	case 0xFE0E, 0xFE0F, 0x200D, 0x200C, 0x200E, 0x200F, 0xFEFF, 0x00AD:
		return true
	}
	return r >= 0xE000 && r <= 0xF8FF
}

// isInvisibleRune reports a character that renders as nothing or reorders text:
// format characters (zero width, bidi controls, BOM, soft hyphen, tag characters
// U+E0000..E007F), variation selectors (FE00..FE0F, E0100..E01EF, 180B..180D),
// the combining grapheme joiner, Hangul and Braille blanks, and private use. The
// project brief strips every one. The write gate holds only the ones that are
// not benign (see isBenignInvisible), plus three or more in a row.
func isInvisibleRune(r rune) bool {
	switch {
	case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r):
		return true
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF, r >= 0x180B && r <= 0x180D:
		return true
	case r >= 0xE0000 && r <= 0xE007F:
		return true
	}
	switch r {
	case 0x034F, 0x115F, 0x1160, 0x3164, 0xFFA0, 0x2800, 0x17B4, 0x17B5:
		return true
	}
	return false
}

// invisibleRunLimit is how many invisible characters in a row count as a
// payload. A flag emoji has two; a zero width binary message has many.
const invisibleRunLimit = 3

// hasInvisibleUnicode reports whether s carries an invisible character the
// write gate refuses: a non-benign one, or a run of invisibleRunLimit of any
// class.
func hasInvisibleUnicode(s string) bool {
	run := 0
	for _, r := range s {
		if !isInvisibleRune(r) {
			run = 0
			continue
		}
		if !isBenignInvisible(r) {
			return true
		}
		if run++; run >= invisibleRunLimit {
			return true
		}
	}
	return false
}

// hasControlRune reports a terminal control character: C0 other than tab,
// newline and carriage return, DEL, and C1. Printed raw they can move the
// cursor, clear the screen or write the clipboard (OSC 52).
func hasControlRune(s string) bool {
	for _, r := range s {
		if (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0x7F || (r >= 0x80 && r <= 0x9F) {
			return true
		}
	}
	return false
}

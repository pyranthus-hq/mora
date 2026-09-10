// Package readable derives a human-reading projection of stored message text.
// It never replaces the stored source: callers keep the original alongside
// the projection and name what was set aside, so a reader can always verify.
// Every rule is deterministic and removes only trailing or non-content
// material; sentences before a closing line are never rewritten.
package readable

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	OmittedQuotedHistory    = "earlier quoted messages"
	OmittedImageMarkup      = "image and link markup"
	OmittedHiddenCharacters = "hidden formatting characters"
	OmittedTrackingMarkup   = "tracking markup"
	OmittedSignature        = "signature"
)

// Result is the reading projection of one message body.
type Result struct {
	Text    string
	Omitted []string
	Changed bool
}

var (
	omittedOrder = []string{OmittedQuotedHistory, OmittedImageMarkup, OmittedHiddenCharacters, OmittedTrackingMarkup, OmittedSignature}
	// Only runes that carry no meaning in running text: preheader padding
	// (U+034F, soft hyphen), zero-width space, word joiner and BOM. Joiners and
	// direction marks stay because emoji sequences and scripts depend on them.
	hiddenRunes    = strings.NewReplacer("\u034f", "", "\u00ad", "", "\u200b", "", "\u2060", "", "\ufeff", "")
	imageMarkup    = regexp.MustCompile(`(?i)\[(?:cid:[^\]]*|image:[^\]]*)\]`)
	encodedImage   = regexp.MustCompile(`\[[A-Za-z0-9+/=]{16,}\]`)
	angleLink      = regexp.MustCompile(`(?i)<https?://[^>\s]*>`)
	trackingLine   = regexp.MustCompile(`^%[A-Za-z0-9_]+%$`)
	outlookDivider = regexp.MustCompile(`^_{8,}$`)
	originalHeader = regexp.MustCompile(`^-{2,}\s*Original Message\s*-{2,}$`)
	forwardHeader  = regexp.MustCompile(`^(?:-{2,}\s*Forwarded message\s*-{2,}|Begin forwarded message:)$`)
	headerFrom     = regexp.MustCompile(`^From:\s.+`)
	headerFollow   = regexp.MustCompile(`^(?:Sent|Date|To|Cc|Subject):\s`)
	wroteLine      = regexp.MustCompile(`^On\s.+`)
	closingLine    = regexp.MustCompile(`(?i)^(?:best|best regards|kind regards|warm regards|warmest regards|regards|thanks|thank you|thanks again|many thanks|cheers|sincerely|sincerely yours|best wishes|warmly|take care|talk soon)[,.!]*$`)
	mobileFooter   = regexp.MustCompile(`^(?:Sent from my (?:iPhone|iPad|Android|Galaxy|Samsung|Pixel|mobile|phone|Mac|BlackBerry)\b.*|Get Outlook for (?:iOS|Android).*)$`)
	sentenceEnd    = regexp.MustCompile(`[.!?]["')\]]*$`)
)

// Email projects one email message body for reading.
func Email(raw string) Result {
	omitted := map[string]bool{}
	text := strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n")
	if stripped := hiddenRunes.Replace(text); stripped != text {
		omitted[OmittedHiddenCharacters] = true
		text = stripped
	}
	text = strings.ReplaceAll(text, "\u00a0", " ")
	lines := strings.Split(text, "\n")
	for i := range lines {
		// Preserve the exact RFC 3676 delimiter through quote/markup removal.
		// An index captured before those removals would point at the wrong line.
		if lines[i] == "-- " {
			lines[i] = signatureDelimiter
			continue
		}
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	lines = cutQuotedHistory(lines, omitted)
	lines = dropMarkup(lines, omitted)
	lines = cutSignature(lines, omitted)
	out := collapse(lines)
	if strings.IndexFunc(out, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	}) < 0 {
		// Nothing readable would remain: show the source untouched, unlabeled.
		return Result{Text: strings.TrimSpace(raw)}
	}
	var labels []string
	for _, label := range omittedOrder {
		if omitted[label] {
			labels = append(labels, label)
		}
	}
	return Result{Text: out, Omitted: labels, Changed: len(labels) > 0}
}

// Bound limits text to a rune count and reports whether it was shortened.
func Bound(text string, limit int) (string, bool) {
	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}
	return string(runes[:limit]), true
}

func cutQuotedHistory(lines []string, omitted map[string]bool) []string {
	cut := len(lines)
	forwarded := len(lines)
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if forwardHeader.MatchString(t) {
			// Forwarded content is the substance of the message, not history:
			// every history rule stops at the banner.
			forwarded = i
			break
		}
		switch {
		case outlookDivider.MatchString(t) && nextNonEmptyMatches(lines, i+1, headerFrom),
			originalHeader.MatchString(t),
			headerFrom.MatchString(t) && nextNonEmptyMatches(lines, i+1, headerFollow),
			wroteLine.MatchString(t) && (strings.HasSuffix(t, "wrote:") || nextNonEmptyEndsWith(lines, i+1, "wrote:")):
			cut = i
		}
		if cut != len(lines) {
			break
		}
	}
	kept := lines[:cut]
	if cut < len(lines) {
		omitted[OmittedQuotedHistory] = true
	}
	out := kept[:0:0]
	for i, line := range kept {
		if i < forwarded && strings.HasPrefix(strings.TrimSpace(line), ">") {
			omitted[OmittedQuotedHistory] = true
			continue
		}
		out = append(out, line)
	}
	return out
}

func nextNonEmptyMatches(lines []string, from int, re *regexp.Regexp) bool {
	for _, line := range lines[min(from, len(lines)):] {
		if t := strings.TrimSpace(line); t != "" {
			return re.MatchString(t)
		}
	}
	return false
}

func nextNonEmptyEndsWith(lines []string, from int, want string) bool {
	for _, line := range lines[min(from, len(lines)):] {
		if t := strings.TrimSpace(line); t != "" {
			return strings.HasSuffix(t, want) && len(t) <= 120
		}
	}
	return false
}

func dropMarkup(lines []string, omitted map[string]bool) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if trackingLine.MatchString(strings.TrimSpace(line)) {
			omitted[OmittedTrackingMarkup] = true
			continue
		}
		cleaned := angleLink.ReplaceAllString(imageMarkup.ReplaceAllString(line, ""), "")
		cleaned = encodedImage.ReplaceAllStringFunc(cleaned, func(token string) string {
			if looksEncoded(token) {
				return ""
			}
			return token
		})
		if cleaned != line {
			omitted[OmittedImageMarkup] = true
		}
		out = append(out, strings.TrimRight(cleaned, " \t"))
	}
	return out
}

// looksEncoded separates base64 image residue from bracketed words: encoded
// data mixes digits with both letter cases and never spells a plain word.
func looksEncoded(token string) bool {
	var digit, upper, lower bool
	for _, r := range token {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		}
	}
	return digit && upper && lower
}

// signatureTail reports whether the lines after a name line read as a
// signature block: few, short, and never sentence-final. A later sentence
// such as "Do not sign." keeps the whole tail as content.
func signatureTail(rest []string) bool {
	count := 0
	for _, line := range rest {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		count++
		if count > 15 || len([]rune(t)) > 120 || sentenceEnd.MatchString(t) {
			return false
		}
	}
	return true
}

const signatureDelimiter = "\x00mora-signature-delimiter"

func cutSignature(lines []string, omitted map[string]bool) []string {
	out := make([]string, 0, len(lines))
	content := false
	for i := 0; i < len(lines); i++ {
		if lines[i] == signatureDelimiter {
			if content {
				omitted[OmittedSignature] = true
				return out
			}
			lines[i] = "--"
		}
		t := strings.TrimSpace(lines[i])
		if t == "" {
			out = append(out, lines[i])
			continue
		}
		if mobileFooter.MatchString(t) {
			omitted[OmittedSignature] = true
			continue
		}
		if content && closingLine.MatchString(t) && signatureTail(lines[i+1:]) {
			out = append(out, lines[i])
			// Keep the name after the closing; set aside titles, contact lines
			// and taglines. The original stays available to the reader.
			rest := lines[i+1:]
			name := -1
			for j, line := range rest {
				if strings.TrimSpace(line) != "" {
					name = j
					break
				}
			}
			if name == -1 {
				return out
			}
			out = append(out, rest[:name]...)
			nameLine := strings.TrimSpace(rest[name])
			if before, _, found := strings.Cut(nameLine, "|"); found {
				omitted[OmittedSignature] = true
				nameLine = strings.TrimSpace(before)
			}
			if mobileFooter.MatchString(nameLine) {
				omitted[OmittedSignature] = true
				return out
			}
			out = append(out, nameLine)
			for _, line := range rest[name+1:] {
				if strings.TrimSpace(line) != "" {
					omitted[OmittedSignature] = true
					break
				}
			}
			return out
		}
		content = true
		out = append(out, lines[i])
	}
	return out
}

func collapse(lines []string) string {
	out := make([]string, 0, len(lines))
	blank := true
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

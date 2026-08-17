package commitment

import (
	"github.com/pyranthus-hq/mora/internal/evidencetext"
	"sort"
	"strings"
)

func Segments(text string) []string {
	authored := evidencetext.SenderAuthoredBody(text)
	if containsAny(strings.ToLower(authored), []string{"earlier request from the archive", "quoted request", "quoted below", "forwarded below", "earlier message below"}) {
		return nil
	}
	var out []string
	for _, raw := range evidencetext.EvidenceSegments(authored) {
		segment := evidencetext.StripNoiseTokens(raw)
		if segment == "" || evidencetext.IsLeadInFragment(segment) || evidencetext.ContainsPersonalTrivia(segment) {
			continue
		}
		out = append(out, evidencetext.OneLine(segment))
	}
	return out
}
func ClosureSegments(text string) []string {
	authored := evidencetext.SenderAuthoredBody(text)
	if authored == "" {
		return nil
	}
	var out []string
	for _, raw := range evidencetext.EvidenceSegments(authored) {
		segment := evidencetext.OneLine(evidencetext.StripNoiseTokens(raw))
		if segment != "" && !evidencetext.IsLeadInFragment(segment) {
			out = append(out, segment)
		}
	}
	return out
}

// FulfilledQuotedRequest recognizes a sender-authored delivery followed by one attributed, quoted request. It returns the source address from names; callers retain provider-identity normalization.
func FulfilledQuotedRequest(body string, blockRefs []string, names map[string]string) (delivery, request, author, blockRef string, ok bool) {
	if len(blockRefs) != 2 {
		return "", "", "", "", false
	}
	lines := strings.Split(body, "\n")
	attribution := -1
	for i, line := range lines {
		if evidencetext.IsQuotedReplyLine(line) {
			attribution = i
			break
		}
	}
	if attribution < 0 {
		return "", "", "", "", false
	}
	delivery = evidencetext.OneLine(evidencetext.SenderAuthoredBody(strings.Join(lines[:attribution], "\n")))
	transition, voice := Transition(delivery)
	if transition != Closed || voice != voiceDelivery {
		return "", "", "", "", false
	}
	attributionLine := strings.ToLower(strings.TrimSpace(lines[attribution]))
	authors := []string{}
	keys := make([]string, 0, len(names))
	for raw := range names {
		keys = append(keys, raw)
	}
	sort.Strings(keys)
	for _, raw := range keys {
		name := strings.ToLower(strings.TrimSpace(names[raw]))
		fields := strings.Fields(name)
		if name == "" || (!strings.Contains(attributionLine, name) && (len(fields) == 0 || len(fields[0]) < 3 || !strings.Contains(attributionLine, fields[0]))) {
			continue
		}
		authors = append(authors, raw)
	}
	if len(authors) != 1 {
		return "", "", "", "", false
	}
	quotedLines := []string{}
	for _, line := range lines[attribution+1:] {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, ">") {
			if trimmed != "" {
				break
			}
			continue
		}
		quotedLines = append(quotedLines, strings.TrimSpace(strings.TrimPrefix(trimmed, ">")))
	}
	segments := Segments(strings.Join(quotedLines, "\n"))
	if len(segments) != 1 || !acceptanceRequest(strings.ToLower(evidencetext.OneLine(segments[0]))) {
		return "", "", "", "", false
	}
	return delivery, segments[0], authors[0], blockRefs[1], true
}

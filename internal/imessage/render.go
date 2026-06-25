package imessage

import (
	"sort"
	"strings"
	"time"
)

// selfLabel is the stable prefix for the user's own sent messages (D-02 — short,
// unambiguous, matches Messages.app's mental model).
const selfLabel = "Me"

// retractedMarker is the terse, honest stand-in for an unsent/retracted message
// (D-12) — chosen over silent omission so the conversational gap stays traceable.
const retractedMarker = "[message removed]"

// messageKind classifies a rendered message so renderLine can apply the D-12
// non-text handling. Tapbacks/reactions are filtered upstream in chatdb, but
// msgSkip is the render-side belt-and-suspenders: a skipped message emits NOTHING
// (not even a day header).
type messageKind int

const (
	msgNormal    messageKind = iota // ordinary text/attachment message
	msgSystem                       // system event → italic line, no sender prefix
	msgRetracted                    // unsent/retracted → [message removed]
	msgSkip                         // tapback/reaction → never rendered (D-12)
)

// imageExts are filename extensions treated as images when the MIME type is absent
// (so a bare "photo.jpg" still renders as [image: …], not a generic attachment).
var imageExts = map[string]bool{
	".heic": true, ".heif": true, ".jpg": true, ".jpeg": true, ".png": true,
	".gif": true, ".webp": true, ".tiff": true, ".bmp": true,
}

// truncationMarker is the EXACT copy rendered (as a `> ` blockquote) at the TOP of a
// bounded conversation body. iMessage keeps the NEWEST messages and drops the oldest,
// so the marker sits ABOVE the first retained day header — the reader must know that
// earlier history was cut (D-03, INVERTED from Gmail's keep-from-start placement).
const truncationMarker = "Truncated — showing the most recent messages of this conversation; older messages were dropped to fit the size budget."

// renderMessage is one message the renderer turns into a `Name: text` line. date is
// the message's real timestamp (Cocoa-epoch converted) — its LOCAL calendar date is
// the day header. sender is the raw handle (resolved to a name at render time, or
// shown raw on no match, D-09); fromMe overrides it with the self label (D-02).
//
// attachments carries metadata-only markers (filename/MIME, never bytes — IMSG-07).
// The attachment/special-message MARKER injection (D-11/D-12) is owned by Plan 02-04;
// this renderer leaves a no-op pass-through hook (renderAttachmentMarkers) so that
// slice fills it without reshaping the renderer.
type renderMessage struct {
	date        time.Time
	fromMe      bool
	sender      string // raw handle; "" for self
	text        string
	attachments []Attachment
	kind        messageKind // D-12 classification; zero value msgNormal
}

// conversation is the title-relevant shape of one chat (D-10). displayName is the
// chat's explicit group name (verbatim title if set). participants are the OTHER
// participants' raw handles in chat.db order (used to synthesize a group title).
// identifier is the 1:1 other-party handle. isGroup distinguishes a group from a 1:1.
type conversation struct {
	displayName  string
	identifier   string
	participants []string
	isGroup      bool
}

// renderResult reports the byte-budget bounding outcome so the mapper can populate
// MappedMemory.Truncated/OriginalSize/IngestedSize (D-04) without re-measuring.
type renderResult struct {
	Truncated    bool
	OriginalSize int
	IngestedSize int
}

// renderTitle produces the memory Title per D-10:
//   - group with an explicit display name → that name verbatim;
//   - group without a name → comma-joined resolved participant names in chat.db order,
//     unknown handles shown raw (D-09);
//   - 1:1 → the other participant's resolved name, else the raw handle.
func renderTitle(c conversation, r *Resolver) string {
	if name := strings.TrimSpace(c.displayName); name != "" {
		return name
	}
	if c.isGroup {
		names := make([]string, 0, len(c.participants))
		for _, h := range c.participants {
			names = append(names, r.Resolve(h))
		}
		return strings.Join(names, ", ")
	}
	// 1:1: prefer the explicit identifier, else the single participant.
	handle := c.identifier
	if handle == "" && len(c.participants) > 0 {
		handle = c.participants[0]
	}
	return r.Resolve(handle)
}

// renderBody produces the day-grouped transcript per UI-SPEC Surface 5 and applies
// the newest-first byte budget (D-03). It returns the rendered body and the bounding
// result. budget <= 0 means unbounded.
//
// Bounding is recency-first: when the full body exceeds budget, the OLDEST messages
// are dropped (newest retained) and the truncation marker is placed at the TOP. All
// size math is rune-safe — messages are dropped whole; the body is NEVER byte-sliced
// mid-rune (emoji/CJK corruption, the project's iMessage Pitfall).
func renderBody(messages []renderMessage, r *Resolver, budget int) (string, renderResult) {
	// Chronological oldest→newest is the canonical order (newest at the bottom, like
	// Messages.app). Sort defensively so callers need not pre-sort.
	msgs := append([]renderMessage(nil), messages...)
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].date.Before(msgs[j].date) })

	full := renderTranscript(msgs, r)
	orig := len([]rune(full)) // rune count: honest size in characters, not bytes

	if budget <= 0 || orig <= budget {
		return full, renderResult{Truncated: false, OriginalSize: orig, IngestedSize: orig}
	}

	// Newest-first: drop whole messages from the FRONT (oldest) until the rendered
	// body — including the marker blockquote — fits the budget. Dropping whole
	// messages keeps every retained line rune-intact (no mid-emoji slice).
	start := 0
	var body string
	for start < len(msgs) {
		kept := msgs[start:]
		body = withMarker(renderTranscript(kept, r))
		if len([]rune(body)) <= budget {
			break
		}
		start++
	}
	if start >= len(msgs) {
		// Even the single newest message + marker exceeds the budget: keep just the
		// newest message under the marker (never emit an empty body for a non-empty
		// conversation; the marker still honestly flags the drop).
		body = withMarker(renderTranscript(msgs[len(msgs)-1:], r))
	}
	return body, renderResult{Truncated: true, OriginalSize: orig, IngestedSize: len([]rune(body))}
}

// withMarker prefixes the truncation blockquote ABOVE the transcript (D-03 — marker
// at the TOP because the dropped content is the OLDEST).
func withMarker(transcript string) string {
	return "> " + truncationMarker + "\n\n" + transcript
}

// renderTranscript builds the day-grouped `## YYYY-MM-DD` sections with `Name: text`
// lines, oldest→newest within each day, a blank line between day sections and no
// blank line between messages within a day (UI-SPEC Surface 5). Input is assumed
// chronologically sorted. Messages that render empty (skipped tapbacks, D-12) emit
// nothing and never open an empty day header.
func renderTranscript(msgs []renderMessage, r *Resolver) string {
	var b strings.Builder
	lastDay := ""
	wroteAny := false
	for _, m := range msgs {
		line := renderLine(m, r)
		if line == "" {
			continue // skipped/empty message — no day header, no line (D-12)
		}
		day := m.date.Local().Format("2006-01-02")
		if day != lastDay {
			if wroteAny {
				b.WriteString("\n") // blank line between day sections
			}
			b.WriteString("## ")
			b.WriteString(day)
			b.WriteString("\n")
			lastDay = day
		}
		b.WriteString(line)
		b.WriteString("\n")
		wroteAny = true
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderLine renders one message, possibly across several lines (D-02/D-09/D-11/D-12):
//   - msgSkip → "" (tapback/reaction never rendered);
//   - msgSystem → a single italic `*text*` line, no sender prefix;
//   - msgRetracted → `<label>: [message removed]` (original text never emitted);
//   - otherwise the `<label>: <text>` line (links render their URL inline as-is),
//     then one `<label>: <marker>` line per attachment (UI-SPEC Surface 5 example).
//
// The user's own messages use the self label (D-02); a no-match handle renders raw
// (D-09). Multi-line message text keeps the prefix on the first line; embedded
// newlines render as-is (no re-prefix). Returns "" for a message with nothing to show.
func renderLine(m renderMessage, r *Resolver) string {
	switch m.kind {
	case msgSkip:
		return ""
	case msgSystem:
		t := strings.TrimSpace(m.text)
		if t == "" {
			return ""
		}
		return "*" + t + "*"
	}

	label := selfLabel
	if !m.fromMe {
		label = r.Resolve(m.sender)
	}

	var lines []string
	if m.kind == msgRetracted {
		// Replace the (possibly still-present) original text with the honest marker.
		lines = append(lines, label+": "+retractedMarker)
	} else if strings.TrimSpace(m.text) != "" {
		lines = append(lines, label+": "+m.text)
	}
	for _, marker := range attachmentMarkers(m.attachments) {
		lines = append(lines, label+": "+marker)
	}
	return strings.Join(lines, "\n")
}

// attachmentMarkers renders each attachment as its own inline marker string (D-11).
func attachmentMarkers(atts []Attachment) []string {
	if len(atts) == 0 {
		return nil
	}
	out := make([]string, 0, len(atts))
	for _, a := range atts {
		out = append(out, attachmentMarker(a))
	}
	return out
}

// attachmentMarker formats one attachment as metadata only — filename + MIME, NEVER
// bytes, payload, or a file path (IMSG-07). Images get the `[image: …]` form; every
// other attachment gets `[attachment: …]` with a ` · ` (space-middot-space) separator
// when both filename and MIME are known.
func attachmentMarker(a Attachment) string {
	name := strings.TrimSpace(a.Filename)
	mime := strings.TrimSpace(a.MimeType)
	// Plugin-payload attachments (rich-link previews, app cards, Tapbacks) carry a
	// bare UUID filename like "<uuid>.pluginPayloadAttachment" with no user-visible
	// signal. Drop the noisy filename so a brief snippet never shows a raw UUID;
	// fall back to MIME (if any) or a generic marker.
	if isPluginPayloadName(name) {
		name = ""
	}
	if isImageAttachment(name, mime) {
		if name != "" {
			return "[image: " + name + "]"
		}
		return "[image: " + mime + "]"
	}
	switch {
	case name != "" && mime != "":
		return "[attachment: " + name + " · " + mime + "]"
	case name != "":
		return "[attachment: " + name + "]"
	case mime != "":
		return "[attachment: " + mime + "]"
	default:
		return "[attachment]"
	}
}

// isPluginPayloadName reports whether a filename is an iMessage plugin-payload
// attachment ("<uuid>.pluginPayloadAttachment") — a rich-message envelope whose
// filename is a content-free UUID that carries no user-visible signal.
func isPluginPayloadName(name string) bool {
	return strings.Contains(strings.ToLower(name), "pluginpayloadattachment")
}

// isImageAttachment is true when the MIME type is an image type, or (absent a MIME
// type) the filename has a known image extension.
func isImageAttachment(name, mime string) bool {
	if strings.HasPrefix(strings.ToLower(mime), "image/") {
		return true
	}
	if mime == "" && name != "" {
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			return imageExts[strings.ToLower(name[i:])]
		}
	}
	return false
}

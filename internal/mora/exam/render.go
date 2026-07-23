package exam

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

const RendererVersion = "exam-render-v1"

// RendererVersionFor names the renderer that a ledger schema binds to. The v1
// string (and the v1 byte stream it stands for) is frozen: the obligations-v1
// corpus hashes are pinned against it.
func RendererVersionFor(schema int) string {
	if schema >= SchemaV3 {
		return "exam-render-v3"
	}
	if schema >= SchemaV2 {
		return "exam-render-v2"
	}
	return RendererVersion
}

var renderLocation = time.FixedZone("exam-render", 0)

type renderedMemory struct {
	id, scope, typ, title, source, createdAt string
	tags, provider, providerID, contentHash  string
	meta                                     map[string]any
	body                                     string
}

type gmailMessageEvidence struct {
	MessageRef string   `json:"message_ref"`
	Sender     string   `json:"sender,omitempty"`
	To         []string `json:"to,omitempty"`
	Cc         []string `json:"cc,omitempty"`
	At         string   `json:"at,omitempty"`
	BlockRefs  []string `json:"block_refs,omitempty"`
}

func Render(l Ledger) (map[string][]byte, error) {
	if err := Validate(l); err != nil {
		return nil, err
	}
	ids := map[string]Identity{l.Self.ID: l.Self}
	for _, p := range l.People {
		ids[p.ID] = p
	}
	out := make(map[string][]byte, len(l.Artifacts))
	for _, a := range l.Artifacts {
		m, path, err := renderArtifact(a, ids, l.Self.ID, l.Version)
		if err != nil {
			return nil, err
		}
		b, err := renderFrontmatter(m)
		if err != nil {
			return nil, err
		}
		if _, exists := out[path]; exists {
			return nil, fmt.Errorf("duplicate rendered path %q", path)
		}
		out[path] = b
	}
	return out, nil
}

func renderArtifact(a Artifact, ids map[string]Identity, selfID string, schema int) (renderedMemory, string, error) {
	providerID := a.MemoryID
	if _, tail, ok := strings.Cut(a.MemoryID, "/"); ok {
		providerID = tail
	}
	m := renderedMemory{id: a.MemoryID, scope: "exam", title: a.Subject, createdAt: a.OccurredAt, contentHash: "fixture-" + strings.NewReplacer("/", "-", "_", "-").Replace(a.ID)}
	switch a.Channel {
	case "gmail":
		m.typ, m.tags, m.provider, m.providerID, m.source = "email", "gmail", "gmail", providerID, providerID
		m.body, m.meta = renderGmail(a, ids, schema)
		return m, "vault/sources/gmail/" + memory.SafeFilename(a.MemoryID) + ".md", nil
	case "imessage":
		m.typ, m.tags, m.provider, m.providerID, m.source = "imessage", "imessage", "imessage", providerID, providerID
		m.body, m.meta = renderIMessage(a, ids, selfID, schema)
		return m, "vault/sources/imessage/" + memory.SafeFilename(a.MemoryID) + ".md", nil
	case "calendar":
		m.typ, m.tags, m.provider, m.providerID, m.source = "event", "calendar", "calendar", providerID, providerID
		m.body, m.meta = renderCalendar(a, ids, selfID, schema)
		return m, "vault/sources/calendar/" + memory.SafeFilename(a.MemoryID) + ".md", nil
	case "notes":
		m.typ, m.source = "note", "manual"
		m.body = renderMessageBody(a.Messages[0], schema)
		return m, "vault/memories/exam/" + memory.SafeFilename(a.MemoryID) + ".md", nil
	default:
		return renderedMemory{}, "", fmt.Errorf("unknown channel %q", a.Channel)
	}
}

func renderGmail(a Artifact, ids map[string]Identity, schema int) (string, map[string]any) {
	from, to, cc, names := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]string{}
	parts := make([]string, 0, len(a.Messages))
	messages := make([]gmailMessageEvidence, 0, len(a.Messages))
	for _, msg := range a.Messages {
		from[identityEmail(ids[msg.From])] = true
		addEmails(to, msg.To, ids)
		addEmails(cc, msg.Cc, ids)
		addNames(names, append(append([]string{msg.From}, msg.To...), msg.Cc...), ids)
		parts = append(parts, renderMessageBody(msg, schema))
		if schema >= SchemaV3 {
			blockRefs := make([]string, 0, len(msg.Body))
			for _, block := range msg.Body {
				blockRefs = append(blockRefs, block.ID)
			}
			at, _ := time.Parse(time.RFC3339, msg.At)
			messages = append(messages, gmailMessageEvidence{
				MessageRef: a.MemoryID + "#" + msg.ID,
				Sender:     identityEmail(ids[msg.From]),
				To:         sortedEmails(msg.To, ids),
				Cc:         sortedEmails(msg.Cc, ids),
				At:         at.UTC().Format(time.RFC3339),
				BlockRefs:  blockRefs,
			})
		}
	}
	meta := map[string]any{"message_count": strconv.Itoa(len(a.Messages)), "occurred_at": newestMessage(a.Messages).UTC().Format(time.RFC3339)}
	putSorted(meta, "from", from)
	putSorted(meta, "to", to)
	putSorted(meta, "cc", cc)
	if len(names) > 0 {
		meta["names"] = names
	}
	if schema >= SchemaV3 && len(messages) > 0 {
		meta["messages"] = messages
		meta["last_sender"] = messages[len(messages)-1].Sender
	}
	first := a.Messages[0]
	return "From: " + identityHeader(ids[first.From]) + "\n\n" + strings.Join(parts, "\n\n---\n\n"), meta
}

func renderIMessage(a Artifact, ids map[string]Identity, selfID string, schema int) (string, map[string]any) {
	var b strings.Builder
	lastDay := ""
	for _, msg := range a.Messages {
		at, _ := time.Parse(time.RFC3339, msg.At)
		day := at.In(renderLocation).Format("2006-01-02")
		if day != lastDay {
			if lastDay != "" {
				b.WriteString("\n")
			}
			b.WriteString("## " + day + "\n")
			lastDay = day
		}
		label := "Me"
		if msg.From != selfID {
			label = identityIMessageLabel(ids[msg.From])
		}
		b.WriteString(label + ": " + renderMessageBody(msg, schema) + "\n")
	}
	pairs := make([]map[string]string, 0, len(a.Participants))
	for _, id := range a.Participants {
		handle := identityHandle(ids[id])
		pairs = append(pairs, map[string]string{"handle": handle, "name": identityIMessageLabel(ids[id])})
	}
	meta := map[string]any{"participants": pairs, "message_count": strconv.Itoa(len(a.Messages)), "occurred_at": newestMessage(a.Messages).UTC().Format(time.RFC3339)}
	return strings.TrimRight(b.String(), "\n"), meta
}

func renderCalendar(a Artifact, ids map[string]Identity, selfID string, schema int) (string, map[string]any) {
	m := a.Messages[0]
	at, _ := time.Parse(time.RFC3339, a.OccurredAt)
	attendees := make([]string, 0, len(m.To))
	names := map[string]string{}
	for _, id := range m.To {
		email := identityEmail(ids[id])
		attendees = append(attendees, email)
		if ids[id].Display != "" {
			names[email] = ids[id].Display
		}
	}
	sort.Strings(attendees)
	var b strings.Builder
	b.WriteString("When: " + at.Format(time.RFC1123) + "\n")
	if len(attendees) > 0 {
		b.WriteString("Attendees: " + strings.Join(attendees, ", ") + "\n")
	}
	body := renderMessageBody(m, schema)
	if body != "" {
		b.WriteString("\n" + body + "\n")
	}
	meta := map[string]any{"attendees": attendees, "occurred_at": at.UTC().Format(time.RFC3339), "organizer": identityEmail(ids[m.From])}
	if self := identityEmail(ids[selfID]); self != "" {
		meta["self_email"] = self
	}
	if ids[m.From].Display != "" {
		names[identityEmail(ids[m.From])] = ids[m.From].Display
	}
	if len(names) > 0 {
		meta["names"] = names
	}
	return strings.TrimRight(b.String(), "\n"), meta
}

func renderBlocks(blocks []Block) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n")
}

// renderMessageBody renders one message's blocks. Schema v1 keeps its frozen
// flat join. Schema v2 makes block kind VISIBLE in the byte stream — the whole
// point of the realism track: what the extractor (and a human reader) sees is
// what real connectors emit, so quote-stripping, footer handling, and wrapped
// mail are finally exercisable by the corpus.
func renderMessageBody(m Message, schema int) string {
	if schema < SchemaV2 {
		return renderBlocks(m.Body)
	}
	parts := make([]string, 0, len(m.Body))
	for _, b := range m.Body {
		text := b.Text
		if m.Wrap > 0 && b.Kind == "authored" {
			text = wrapText(text, m.Wrap)
		}
		switch b.Kind {
		case "quoted_reply":
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				lines[i] = "> " + line
			}
			quoted := strings.Join(lines, "\n")
			if b.Attr != "" {
				quoted = b.Attr + "\n" + quoted
			}
			parts = append(parts, quoted)
		case "forwarded":
			attr := b.Attr
			if attr == "" {
				attr = "---------- Forwarded message ---------"
			}
			parts = append(parts, attr+"\n"+text)
		case "signature":
			parts = append(parts, "-- \n"+text)
		default:
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// wrapText greedily hard-wraps each line at width columns (counted in runes),
// the way 72-column mail clients do. Lines that already fit pass through
// verbatim — wrapping must never rewrite text it did not need to touch. Words
// longer than the width are left unbroken — a URL that got split across lines
// is a corpus defect (#136's shape), not a renderer's job to manufacture.
func wrapText(text string, width int) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if len([]rune(line)) <= width {
			out = append(out, line)
			continue
		}
		words := strings.Fields(line)
		current := words[0]
		for _, word := range words[1:] {
			if len([]rune(current))+1+len([]rune(word)) > width {
				out = append(out, current)
				current = word
				continue
			}
			current += " " + word
		}
		out = append(out, current)
	}
	return strings.Join(out, "\n")
}

func renderFrontmatter(m renderedMemory) ([]byte, error) {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\nscope: %s\ntype: %s\ntitle: %s\n", m.id, m.scope, m.typ, quoteYAML(m.title))
	fmt.Fprintf(&b, "tags: [%s]\nsource: %s\ncreated_at: %s\n", m.tags, quoteYAML(m.source), m.createdAt)
	if m.provider != "" {
		fmt.Fprintf(&b, "provider: %s\nprovider_id: %s\n", m.provider, quoteYAML(m.providerID))
	}
	if m.contentHash != "" {
		fmt.Fprintf(&b, "content_hash: %s\n", m.contentHash)
	}
	metaJSON, err := memory.CanonicalMeta(m.meta)
	if err != nil {
		return nil, err
	}
	if metaJSON != "" {
		fmt.Fprintf(&b, "meta: %s\n", metaJSON)
	}
	fmt.Fprintf(&b, "---\n\n%s\n", m.body)
	return []byte(b.String()), nil
}

func quoteYAML(s string) string {
	if strings.ContainsAny(s, ":#[]") {
		return strconv.Quote(s)
	}
	return s
}

func newestMessage(messages []Message) time.Time {
	var newest time.Time
	for _, m := range messages {
		at, _ := time.Parse(time.RFC3339, m.At)
		if at.After(newest) {
			newest = at
		}
	}
	return newest
}

func identityEmail(id Identity) string {
	if len(id.Emails) == 0 {
		return ""
	}
	return strings.ToLower(id.Emails[0])
}

func identityHandle(id Identity) string {
	if len(id.Handles) == 0 {
		return ""
	}
	return id.Handles[0]
}

func identityHeader(id Identity) string {
	email := identityEmail(id)
	if id.Display == "" {
		return email
	}
	return id.Display + " <" + email + ">"
}

func identityIMessageLabel(id Identity) string {
	if id.Display != "" {
		return id.Display
	}
	return identityHandle(id)
}

func addEmails(dst map[string]bool, refs []string, ids map[string]Identity) {
	for _, ref := range refs {
		if email := identityEmail(ids[ref]); email != "" {
			dst[email] = true
		}
	}
}

func sortedEmails(refs []string, ids map[string]Identity) []string {
	values := make([]string, 0, len(refs))
	for _, ref := range refs {
		if email := identityEmail(ids[ref]); email != "" {
			values = append(values, email)
		}
	}
	sort.Strings(values)
	return values
}

func addNames(dst map[string]string, refs []string, ids map[string]Identity) {
	for _, ref := range refs {
		if ids[ref].Display != "" && identityEmail(ids[ref]) != "" {
			dst[identityEmail(ids[ref])] = ids[ref].Display
		}
	}
}

func putSorted(meta map[string]any, key string, values map[string]bool) {
	if len(values) == 0 {
		return
	}
	out := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	if len(out) > 0 {
		meta[key] = out
	}
}

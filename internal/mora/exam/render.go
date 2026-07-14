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

type renderedMemory struct {
	id, scope, typ, title, source, createdAt string
	tags, provider, providerID, contentHash  string
	meta                                     map[string]any
	body                                     string
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
		m, path, err := renderArtifact(a, ids, l.Self.ID)
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

func renderArtifact(a Artifact, ids map[string]Identity, selfID string) (renderedMemory, string, error) {
	providerID := a.MemoryID
	if _, tail, ok := strings.Cut(a.MemoryID, "/"); ok {
		providerID = tail
	}
	m := renderedMemory{id: a.MemoryID, scope: "exam", title: a.Subject, createdAt: a.OccurredAt, contentHash: "fixture-" + strings.NewReplacer("/", "-", "_", "-").Replace(a.ID)}
	switch a.Channel {
	case "gmail":
		m.typ, m.tags, m.provider, m.providerID, m.source = "email", "gmail", "gmail", providerID, providerID
		m.body, m.meta = renderGmail(a, ids)
		return m, "vault/sources/gmail/" + memory.SafeFilename(a.MemoryID) + ".md", nil
	case "imessage":
		m.typ, m.tags, m.provider, m.providerID, m.source = "imessage", "imessage", "imessage", providerID, providerID
		m.body, m.meta = renderIMessage(a, ids, selfID)
		return m, "vault/sources/imessage/" + memory.SafeFilename(a.MemoryID) + ".md", nil
	case "calendar":
		m.typ, m.tags, m.provider, m.providerID, m.source = "event", "calendar", "calendar", providerID, providerID
		m.body, m.meta = renderCalendar(a, ids, selfID)
		return m, "vault/sources/calendar/" + memory.SafeFilename(a.MemoryID) + ".md", nil
	case "notes":
		m.typ, m.source = "note", "manual"
		m.body = renderBlocks(a.Messages[0].Body)
		return m, "vault/memories/exam/" + memory.SafeFilename(a.MemoryID) + ".md", nil
	default:
		return renderedMemory{}, "", fmt.Errorf("unknown channel %q", a.Channel)
	}
}

func renderGmail(a Artifact, ids map[string]Identity) (string, map[string]any) {
	from, to, cc, names := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]string{}
	parts := make([]string, 0, len(a.Messages))
	for _, msg := range a.Messages {
		from[identityEmail(ids[msg.From])] = true
		addEmails(to, msg.To, ids)
		addEmails(cc, msg.Cc, ids)
		addNames(names, append(append([]string{msg.From}, msg.To...), msg.Cc...), ids)
		parts = append(parts, renderBlocks(msg.Body))
	}
	meta := map[string]any{"message_count": strconv.Itoa(len(a.Messages)), "occurred_at": newestMessage(a.Messages).UTC().Format(time.RFC3339)}
	putSorted(meta, "from", from)
	putSorted(meta, "to", to)
	putSorted(meta, "cc", cc)
	if len(names) > 0 {
		meta["names"] = names
	}
	first := a.Messages[0]
	return "From: " + identityHeader(ids[first.From]) + "\n\n" + strings.Join(parts, "\n\n---\n\n"), meta
}

func renderIMessage(a Artifact, ids map[string]Identity, selfID string) (string, map[string]any) {
	var b strings.Builder
	lastDay := ""
	for _, msg := range a.Messages {
		at, _ := time.Parse(time.RFC3339, msg.At)
		day := at.Local().Format("2006-01-02")
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
		b.WriteString(label + ": " + renderBlocks(msg.Body) + "\n")
	}
	pairs := make([]map[string]string, 0, len(a.Participants))
	for _, id := range a.Participants {
		handle := identityHandle(ids[id])
		pairs = append(pairs, map[string]string{"handle": handle, "name": identityIMessageLabel(ids[id])})
	}
	meta := map[string]any{"participants": pairs, "message_count": strconv.Itoa(len(a.Messages)), "occurred_at": newestMessage(a.Messages).UTC().Format(time.RFC3339)}
	return strings.TrimRight(b.String(), "\n"), meta
}

func renderCalendar(a Artifact, ids map[string]Identity, selfID string) (string, map[string]any) {
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
	body := renderBlocks(m.Body)
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

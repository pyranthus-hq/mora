// Package activity derives connector-neutral activity facts from one memory.
//
// It only reads the supplied memory. Callers provide now so the result is
// deterministic and can use Select for a stable, source-event ordering.
package activity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/segments"
)

// EventSource identifies the evidence used for EventAt.
type EventSource string

const (
	EventSourceMessageEvidence EventSource = "message_evidence"
	EventSourceOccurredAt      EventSource = "occurred_at"
)

// Participation is the shared evidence-backed conversation DTO.
type Participation = memory.Participation

// Projection is the read-time activity projection for one memory. Automated is
// nil unless affirmative, validated automation evidence exists.
type Projection struct {
	EventAt       *time.Time
	EventSource   EventSource
	Eligible      bool
	Participation *Participation
	Automated     *bool
	// AutomationBasis names the validated evidence supporting Automated. It is
	// empty when automation is unknown.
	AutomationBasis string
}

// Result joins a memory with its activity projection. Select orders Results by
// EventAt descending and Memory.ID ascending for equal event instants.
type Result struct {
	Memory     memory.Memory
	Projection Projection
}

// Derive selects activity time and participation without reading sources or
// writing a vault. It supports Gmail, iMessage, WhatsApp, Google Calendar, and
// Apple Calendar. Conversation memories prefer the newest validated retained
// message-evidence timestamp; only when no such timestamp exists do they fall
// back to explicit meta.occurred_at. Calendar memories use occurred_at only.
//
// An unknown, malformed, or future selected time is ineligible. now itself is
// inclusive: an event at now is eligible. CreatedAt is never a fallback.
func Derive(m memory.Memory, now time.Time) Projection {
	p := deriveEvidence(m)
	if stamped, ok := validStamp(m); ok {
		// A stamp is generated from the same retained evidence. Prefer it only
		// when its complete, versioned shape validates; bad or stale metadata
		// must never replace live derivation.
		p = stamped
	}
	if p.EventAt != nil && !p.EventAt.After(now) {
		p.Eligible = true
	}
	return p
}

// StampMeta returns the durable, content-free activity facts a connector may
// persist for a newly mapped memory. It does not read a provider or write a
// vault. The stamp is intentionally a map so it remains frontmatter-safe.
func StampMeta(m memory.Memory) map[string]any {
	p := deriveEvidence(m)
	stamp := map[string]any{"version": 1, "automated": nil, "automation_basis": "", "evidence_fingerprint": evidenceFingerprint(m, p)}
	if p.EventAt != nil {
		stamp["event_at"] = p.EventAt.UTC().Format(time.RFC3339Nano)
		stamp["event_source"] = string(p.EventSource)
	}
	if p.Participation != nil {
		stamp["participation"] = p.Participation
	}
	if p.Automated != nil {
		stamp["automated"] = *p.Automated
		stamp["automation_basis"] = p.AutomationBasis
	}
	return stamp
}

func deriveEvidence(m memory.Memory) Projection {
	p := Projection{}
	provider := providerOf(m)
	if !supported(provider) {
		return p
	}
	var rows []segments.Row
	switch provider {
	case "imessage", "whatsapp":
		if !m.Truncated {
			rows, _ = segments.Derive(m)
		}
		if len(rows) > 0 {
			p.Participation = participation(rows, explicitGroup(m, provider))
		}
	case "gmail":
		rows, _ = segments.Derive(m)
	}
	if latest, ok := newestRow(rows); ok {
		at := latest.at
		p.EventAt, p.EventSource = &at, EventSourceMessageEvidence
		p.Automated, p.AutomationBasis = automation(m, provider, latest.row)
	}
	if p.EventAt == nil {
		if at, ok := occurredAt(m); ok {
			p.EventAt, p.EventSource = at, EventSourceOccurredAt
		}
	}
	return p
}

type newestEvidence struct {
	row segments.Row
	at  time.Time
}

func newestRow(rows []segments.Row) (newestEvidence, bool) {
	var newest newestEvidence
	ok := false
	for _, row := range rows {
		at, valid := parseTime(row.At)
		if !valid {
			continue
		}
		if !ok || at.After(newest.at) || (at.Equal(newest.at) && row.EvidenceRef < newest.row.EvidenceRef) {
			newest, ok = newestEvidence{row: row, at: *at}, true
		}
	}
	return newest, ok
}

// Select derives eligible activity rows and returns a new deterministic order.
func Select(memories []memory.Memory, now time.Time) []Result {
	return SelectRange(memories, time.Time{}, now)
}

// SelectRange returns eligible activity rows in the inclusive [from, now] range.
// A zero from disables the lower bound.
func SelectRange(memories []memory.Memory, from, now time.Time) []Result {
	out := make([]Result, 0, len(memories))
	for _, m := range memories {
		p := Derive(m, now)
		if p.Eligible && (from.IsZero() || !p.EventAt.Before(from)) {
			out = append(out, Result{Memory: m, Projection: p})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.Projection.EventAt.Equal(*b.Projection.EventAt) {
			return a.Projection.EventAt.After(*b.Projection.EventAt)
		}
		return a.Memory.ID < b.Memory.ID
	})
	return out
}

func providerOf(m memory.Memory) string {
	for _, value := range []string{m.Provider, m.Type, m.Source} {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			if value == "applecal" {
				return "applecalendar"
			}
			return value
		}
	}
	return ""
}

func supported(provider string) bool {
	switch provider {
	case "gmail", "imessage", "whatsapp", "calendar", "applecalendar":
		return true
	default:
		return false
	}
}

// Supports reports whether the memory's connector identity is eligible for
// activity projection. It uses the same Provider/Type/Source fallback as Derive.
func Supports(m memory.Memory) bool { return supported(providerOf(m)) }

// ValidAutomationBasis reports whether a positive automation basis is one this
// version can safely project from a disposable stamp.
func ValidAutomationBasis(basis string) bool {
	switch basis {
	case "sender_shortcode", "sender_tollfree", "sender_pattern", "header_list_unsubscribe", "header_precedence":
		return true
	default:
		return false
	}
}

func occurredAt(m memory.Memory) (*time.Time, bool) {
	if m.Meta == nil {
		return nil, false
	}
	value, ok := m.Meta["occurred_at"].(string)
	if !ok {
		return nil, false
	}
	return parseTime(value)
}

func participation(rows []segments.Row, group *bool) *Participation {
	p := &Participation{MessageEvidenceCount: len(rows), IsGroup: group}
	var own int
	var latest *time.Time
	var latestRef string
	var lastOwnRef string
	for _, row := range rows {
		at, ok := parseTime(row.At)
		if !ok { // defensive: segments currently validates these for conversations.
			continue
		}
		if p.LatestSender == "" || latest == nil || at.After(*latest) || (at.Equal(*latest) && row.EvidenceRef < latestRef) {
			p.LatestSender, latest, latestRef = row.Sender, at, row.EvidenceRef
		}
		if segments.Direction(row.BlockRefs) != "outgoing" {
			continue
		}
		own++
		if p.LastOwnAt == nil || at.After(*p.LastOwnAt) || (at.Equal(*p.LastOwnAt) && row.EvidenceRef < lastOwnRef) {
			copy := *at
			p.LastOwnAt = &copy
			lastOwnRef = row.EvidenceRef
		}
	}
	p.OwnShare = float64(own) / float64(p.MessageEvidenceCount)
	return p
}

func explicitGroup(m memory.Memory, provider string) *bool {
	if m.Meta == nil {
		return nil
	}
	if provider == "imessage" {
		if value, ok := m.Meta["is_group"].(bool); ok {
			return &value
		}
		return nil
	}
	if value, ok := m.Meta["chat_kind"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "group":
			v := true
			return &v
		case "direct":
			v := false
			return &v
		}
	}
	return nil
}

// automation recognizes only affirmative, documented sender/header evidence.
// Lack of a match is unknown, not a human/false classification.
func automation(m memory.Memory, provider string, row segments.Row) (*bool, string) {
	sender := strings.TrimSpace(row.Sender)
	if sender == "" {
		return nil, ""
	}
	if headers := automationHeaders(m, row.EvidenceRef); len(headers) > 0 {
		v := true
		return &v, "header_" + headers[0]
	}
	// Resolver names may carry (smsfp). A named contact is not an automation
	// signal merely because it came from the SMS fingerprint store.
	base := sender
	if i := strings.Index(strings.ToLower(base), "(smsfp)"); i >= 0 {
		before := strings.TrimSpace(base[:i])
		if containsLetter(before) {
			return nil, ""
		}
		base = before
	}
	if isShortCode(base) {
		v := true
		return &v, "sender_shortcode"
	}
	if isTollFree(base) {
		v := true
		return &v, "sender_tollfree"
	}
	if provider == "gmail" && isAutomatedAddress(base) {
		v := true
		return &v, "sender_pattern"
	}
	return nil, ""
}
func containsLetter(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}
func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func isShortCode(s string) bool {
	d := digitsOnly(s)
	return d == strings.TrimSpace(s) && len(d) >= 5 && len(d) <= 6
}
func isTollFree(s string) bool {
	d := digitsOnly(s)
	if len(d) == 11 && d[0] == '1' {
		d = d[1:]
	}
	if len(d) != 10 {
		return false
	}
	switch d[:3] {
	case "800", "833", "844", "855", "866", "877", "888":
		return true
	}
	return false
}
func isAutomatedAddress(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "<"); i >= 0 && strings.HasSuffix(s, ">") {
		s = strings.TrimSpace(s[i+1 : len(s)-1])
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 {
		return false
	}
	local := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s[:at], "-", ""), "_", ""), ".", "")
	return strings.HasPrefix(local, "noreply") || strings.HasPrefix(local, "donotreply") || strings.HasPrefix(local, "notification") || strings.HasPrefix(local, "alert")
}

// automationHeaders returns normalized, positive header facts only for the
// selected Gmail message. Old mail without retained facts remains unknown.
func automationHeaders(m memory.Memory, ref string) []string {
	if m.Meta == nil {
		return nil
	}
	b, err := json.Marshal(m.Meta["messages"])
	if err != nil {
		return nil
	}
	var rows []struct {
		MessageRef string   `json:"message_ref"`
		Headers    []string `json:"automation_headers"`
	}
	if json.Unmarshal(b, &rows) != nil {
		return nil
	}
	for _, row := range rows {
		if row.MessageRef == ref {
			return positiveHeaders(row.Headers)
		}
	}
	return nil
}
func positiveHeaders(headers []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range headers {
		h = strings.ToLower(strings.TrimSpace(h))
		var basis string
		switch h {
		case "list-unsubscribe":
			basis = "list_unsubscribe"
		case "precedence: bulk", "precedence: list", "precedence: junk":
			basis = "precedence"
		}
		if basis != "" && !seen[basis] {
			seen[basis] = true
			out = append(out, basis)
		}
	}
	sort.Strings(out)
	return out
}

// evidenceFingerprint binds a stamp to the currently retained, validated
// evidence without exposing message text. It deliberately excludes an account
// suffix on Gmail parent IDs: account tagging may happen after mapping, while
// the underlying message evidence has not changed.
func evidenceFingerprint(m memory.Memory, p Projection) string {
	event := ""
	if p.EventAt != nil {
		event = p.EventAt.UTC().Format(time.RFC3339Nano)
	}
	part := ""
	if p.Participation != nil {
		part = fmt.Sprintf("%.17g|%s|%t|%t|%s|%d", p.Participation.OwnShare, timeString(p.Participation.LastOwnAt), boolValue(p.Participation.IsGroup), p.Participation.IsGroup != nil, p.Participation.LatestSender, p.Participation.MessageEvidenceCount)
	}
	auto := ""
	if p.Automated != nil {
		auto = fmt.Sprintf("%t", *p.Automated)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{providerOf(m), event, string(p.EventSource), part, auto, p.AutomationBasis}, "\x00")))
	return hex.EncodeToString(sum[:])
}
func timeString(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func boolValue(v *bool) bool { return v != nil && *v }

func validStamp(m memory.Memory) (Projection, bool) {
	if m.Meta == nil {
		return Projection{}, false
	}
	raw, ok := m.Meta["activity_stamp"]
	if !ok {
		return Projection{}, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return Projection{}, false
	}
	var stamp struct {
		Version       json.Number    `json:"version"`
		EventAt       string         `json:"event_at"`
		EventSource   EventSource    `json:"event_source"`
		Participation *Participation `json:"participation"`
		Automated     *bool          `json:"automated"`
		Basis         string         `json:"automation_basis"`
		Fingerprint   string         `json:"evidence_fingerprint"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if dec.Decode(&stamp) != nil {
		return Projection{}, false
	}
	version, err := stamp.Version.Int64()
	if err != nil || version != 1 || strings.TrimSpace(stamp.Fingerprint) == "" {
		return Projection{}, false
	}
	rawProjection := deriveEvidence(m)
	if stamp.Fingerprint != evidenceFingerprint(m, rawProjection) {
		return Projection{}, false
	}
	p := Projection{Participation: stamp.Participation, Automated: stamp.Automated, AutomationBasis: strings.TrimSpace(stamp.Basis), EventSource: stamp.EventSource}
	if stamp.EventAt != "" {
		at, ok := parseTime(stamp.EventAt)
		if !ok {
			return Projection{}, false
		}
		p.EventAt = at
	}
	if p.EventAt == nil || (p.EventSource != EventSourceMessageEvidence && p.EventSource != EventSourceOccurredAt) {
		return Projection{}, false
	}
	if p.Automated != nil && p.AutomationBasis == "" {
		return Projection{}, false
	}
	if p.Automated == nil && p.AutomationBasis != "" {
		return Projection{}, false
	}
	if !validParticipation(p.Participation) {
		return Projection{}, false
	}
	// Binding must cover the decoded stamp too. Checking only raw evidence
	// would let a caller alter stamp fields while retaining its old digest.
	if stamp.Fingerprint != evidenceFingerprint(m, p) {
		return Projection{}, false
	}
	return p, true
}
func validParticipation(p *Participation) bool {
	if p == nil {
		return true
	}
	return p.MessageEvidenceCount > 0 && p.OwnShare >= 0 && p.OwnShare <= 1 && !math.IsNaN(p.OwnShare) && !math.IsInf(p.OwnShare, 0) && strings.TrimSpace(p.LatestSender) != ""
}

func parseTime(value string) (*time.Time, bool) {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, false
	}
	return &at, true
}

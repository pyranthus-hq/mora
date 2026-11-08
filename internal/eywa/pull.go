package eywa

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// MaxLookback is the oldest `since` the change feed accepts.
const MaxLookback = 35 * 24 * time.Hour

// Version is one fact version as the agent API returns it.
type Version struct {
	VersionID  string  `json:"version_id"`
	FactID     string  `json:"fact_id"`
	Entity     string  `json:"entity"`
	Attribute  string  `json:"attribute"`
	Value      string  `json:"value"`
	Topic      string  `json:"topic"`
	Visibility string  `json:"visibility"`
	Status     string  `json:"status"`
	ValidFrom  string  `json:"valid_from"`
	ValidTo    *string `json:"valid_to"`
	RecordedAt string  `json:"recorded_at"`
	RetiredAt  *string `json:"retired_at"`
	SourceURI  string  `json:"source_uri"` // history rows carry the URI flat
	Lineage    *struct {
		SourceURI   string  `json:"source_uri"`
		ContentHash string  `json:"content_hash"`
		Signer      string  `json:"signer"`
		Excerpt     *string `json:"excerpt"`
	} `json:"lineage"`
}

// Known is what the vault already holds about a version, so a later sync can
// resolve a replacement it only sees by id and never loses lineage it fetched
// earlier (history rows carry the source URI only).
type Known struct {
	FactID      string
	Entity      string
	Attribute   string
	ContentHash string
	Signer      string
	Excerpt     string
	ReplacedBy  string
}

// Snapshot is the read-only record carried to the vault for one version.
type Snapshot struct {
	VersionID   string
	FactID      string
	Entity      string
	Attribute   string
	Value       string
	Status      string // "current" or "replaced" (the service's "retired"), else the raw value
	RawStatus   string
	ValidFrom   string
	ValidTo     string
	RecordedAt  string
	RetiredAt   string
	ReplacedBy  string
	Topic       string
	Visibility  string
	SourceURI   string
	ContentHash string
	Signer      string
	Excerpt     string
	RetrievedAt string
}

// RecordFailure is a per-record failure carried through the ingest loop so it
// is counted and surfaced like any other dropped item, never swallowed.
type RecordFailure struct {
	Ref string
	Err string
}

// PullStats counts what a sync saw but did not turn into records.
type PullStats struct {
	Skipped    int // replacements of versions this vault never pulled
	OwnUpdates int // status changes of this agent's own updates (read with `mora eywa status`)
}

// Fetcher pages the change feed and resolves each change into version
// records. It implements memory.Fetcher and memory.ContextFetcher.
type Fetcher struct {
	client *Client
	lookup func(versionID string) (Known, bool)
	now    func() time.Time
	seen   map[string]Known
	Stats  PullStats
}

// NewFetcher builds a pull fetcher. lookup reports what the vault already
// holds for a version id (nil = nothing known).
func NewFetcher(client *Client, lookup func(string) (Known, bool), now func() time.Time) *Fetcher {
	if now == nil {
		now = time.Now
	}
	if lookup == nil {
		lookup = func(string) (Known, bool) { return Known{}, false }
	}
	return &Fetcher{client: client, lookup: lookup, now: now, seen: map[string]Known{}}
}

type change struct {
	ChangeID string            `json:"change_id"`
	Type     string            `json:"type"`
	Topic    *string           `json:"topic"`
	IDs      map[string]string `json:"ids"`
	Status   string            `json:"status"`
	Own      bool              `json:"own"`
}

type changesPage struct {
	Items   []change `json:"items"`
	Next    string   `json:"next"`
	HasMore bool     `json:"has_more"`
}

// FetchPage is the legacy seam.
func (f *Fetcher) FetchPage(kind memory.ItemKind, w memory.FetchWindow, cursor string) (memory.Page, error) {
	return f.FetchPageContext(context.Background(), kind, w, cursor)
}

// FetchPageContext reads one change-feed page. The first sync starts at
// window.Since (clamped to the feed's 35-day horizon); later syncs resume from
// the persisted cursor (window.SyncCursor). Every page reports the feed's next
// cursor as the SyncCursor, so the ingest loop commits it only after a clean walk.
func (f *Fetcher) FetchPageContext(ctx context.Context, kind memory.ItemKind, w memory.FetchWindow, cursor string) (memory.Page, error) {
	if kind != KindFact {
		return memory.Page{}, fmt.Errorf("eywa: unsupported kind %q", kind)
	}
	args := map[string]any{}
	fromSyncCursor := false
	switch {
	case cursor != "":
		args["cursor"] = cursor
	case w.SyncCursor != "":
		args["cursor"] = w.SyncCursor
		fromSyncCursor = true
	default:
		since := w.Since
		horizon := f.now().Add(-MaxLookback + time.Hour)
		if since.IsZero() || since.Before(horizon) {
			since = horizon
		}
		args["since"] = since.UTC().Format(time.RFC3339)
	}
	var page changesPage
	if err := f.client.CallTool(ctx, "changes_since", args, &page); err != nil {
		var rpc *RPCError
		if fromSyncCursor && errors.As(err, &rpc) && rpc.Code == rpcInvalidParams {
			// The stored cursor is no longer accepted: fall back to a fresh walk.
			return memory.Page{}, fmt.Errorf("eywa: stored change cursor rejected: %w", memory.ErrIncrementalCursorExpired)
		}
		return memory.Page{}, asAgentFailure(err)
	}
	retrieved := f.now().UTC().Format(time.RFC3339)
	items, err := f.resolve(ctx, page.Items, retrieved)
	if err != nil {
		return memory.Page{}, err
	}
	out := memory.Page{Items: items, SyncCursor: page.Next}
	if page.HasMore && page.Next != "" {
		out.NextCursor = page.Next
	}
	return out, nil
}

type slotKey struct{ entity, attribute string }

// resolve turns one page of ids into version items. Transient and agent-level
// failures abort the page (the checkpoint is preserved); a per-record refusal
// becomes a RecordFailure item. Admissions are resolved first, so a
// replacement later on the same page finds the version it replaces.
func (f *Fetcher) resolve(ctx context.Context, changes []change, retrieved string) ([]memory.Item, error) {
	var order []slotKey
	slots := map[slotKey]*Version{}
	var failures []memory.Item
	addSlot := func(k slotKey, current *Version) {
		if prev, ok := slots[k]; ok {
			if prev == nil && current != nil {
				slots[k] = current
			}
			return
		}
		slots[k] = current
		order = append(order, k)
	}
	refFor := func(versionID string) (Known, bool) {
		if versionID == "" {
			return Known{}, false
		}
		if k, ok := f.seen[versionID]; ok {
			return k, true
		}
		return f.lookup(versionID)
	}
	var deferred []change
	for _, ch := range changes {
		switch ch.Type {
		case "fact.admitted":
			factID := ch.IDs["fact_id"]
			if factID == "" {
				if k, ok := refFor(ch.IDs["version_id"]); ok {
					addSlot(slotKey{k.Entity, k.Attribute}, nil)
					continue
				}
				f.Stats.Skipped++
				continue
			}
			var got struct {
				Version *Version `json:"version"`
			}
			err := f.client.CallTool(ctx, "get_memory", map[string]any{"fact_id": factID}, &got)
			if err != nil {
				if k, ok := refFor(ch.IDs["version_id"]); ok && isPerRecord(err) {
					// No current version any more (replaced or withdrawn): history still answers.
					addSlot(slotKey{k.Entity, k.Attribute}, nil)
					continue
				}
				if isPerRecord(err) {
					failures = append(failures, failureItem("fact "+factID, err))
					continue
				}
				return nil, err
			}
			if got.Version == nil || got.Version.Entity == "" || got.Version.Attribute == "" {
				failures = append(failures, failureItem("fact "+factID, errors.New("eywa: get_memory returned no readable version")))
				continue
			}
			addSlot(slotKey{got.Version.Entity, got.Version.Attribute}, got.Version)
		case "version.superseded", "version.promoted":
			deferred = append(deferred, ch)
		case "write.status":
			f.Stats.OwnUpdates++
		default:
			f.Stats.Skipped++
		}
	}
	items := make([]memory.Item, 0, len(order)+len(failures))
	byVersion := map[string]int{}
	resolved := 0
	resolvePending := func() error {
		for ; resolved < len(order); resolved++ {
			k := order[resolved]
			slotItems, err := f.resolveSlot(ctx, k, slots[k], retrieved)
			if err != nil {
				if isPerRecord(err) {
					items = append(items, failureItem(fmt.Sprintf("%s / %s", k.entity, k.attribute), err))
					continue
				}
				return err
			}
			for _, it := range slotItems {
				byVersion[it.ProviderID] = len(items)
				items = append(items, it)
			}
		}
		return nil
	}
	if err := resolvePending(); err != nil {
		return nil, err
	}
	for _, ch := range deferred {
		if ch.Type == "version.promoted" {
			id := ch.IDs["version_id"]
			if _, done := byVersion[id]; done {
				continue
			}
			if k, ok := refFor(id); ok {
				addSlot(slotKey{k.Entity, k.Attribute}, nil)
				continue
			}
			f.Stats.Skipped++
			continue
		}
		oldID, newID := ch.IDs["old_version_id"], ch.IDs["new_version_id"]
		if idx, done := byVersion[oldID]; done {
			// Already rendered on this page: record which version replaced it.
			if snap, ok := items[idx].Payload.(Snapshot); ok && snap.Status == "replaced" && newID != "" {
				snap.ReplacedBy = newID
				items[idx] = SnapshotItem(snap)
				known := f.seen[oldID]
				known.ReplacedBy = newID
				f.seen[oldID] = known
			}
			continue
		}
		k, ok := refFor(oldID)
		if !ok {
			k, ok = refFor(newID)
		}
		if !ok {
			f.Stats.Skipped++
			continue
		}
		if oldID != "" && newID != "" {
			known := k
			if prior, had := refFor(oldID); had {
				known = prior
			}
			known.ReplacedBy = newID
			f.seen[oldID] = known
		}
		addSlot(slotKey{k.Entity, k.Attribute}, nil)
	}
	if err := resolvePending(); err != nil {
		return nil, err
	}
	return append(items, failures...), nil
}

// isPerRecord reports a refusal scoped to one lookup (Forbidden, a tool error).
// Auth, rate-limit, transport and availability failures are never per record.
func isPerRecord(err error) bool {
	var fe *ForbiddenError
	var te *ToolError
	return errors.As(err, &fe) || errors.As(err, &te)
}

func failureItem(ref string, err error) memory.Item {
	return memory.Item{Kind: KindFact, ProviderID: "failed/" + ref, Payload: RecordFailure{Ref: ref, Err: err.Error()}}
}

// resolveSlot reads every version of one (entity, attribute) and the current
// version's full lineage, and returns one item per version.
func (f *Fetcher) resolveSlot(ctx context.Context, k slotKey, current *Version, retrieved string) ([]memory.Item, error) {
	if current == nil {
		var got struct {
			Version *Version `json:"version"`
		}
		err := f.client.CallTool(ctx, "get_memory", map[string]any{"entity": k.entity, "attribute": k.attribute}, &got)
		switch {
		case err == nil:
			current = got.Version
		case isPerRecord(err):
			// No current version is a legitimate state (every version retired).
		default:
			return nil, err
		}
	}
	var hist struct {
		Entity   string    `json:"entity"`
		Versions []Version `json:"versions"`
	}
	if err := f.client.CallTool(ctx, "history", map[string]any{"entity": k.entity, "attribute": k.attribute}, &hist); err != nil {
		return nil, err
	}
	byID := map[string]Version{}
	for _, v := range hist.Versions {
		if v.Attribute != "" && !strings.EqualFold(v.Attribute, k.attribute) {
			continue
		}
		v.Entity, v.Attribute = k.entity, k.attribute
		byID[v.VersionID] = v
	}
	if current != nil && current.VersionID != "" {
		merged := byID[current.VersionID]
		if merged.VersionID == "" {
			merged = *current
		}
		merged.FactID = firstNonEmpty(current.FactID, merged.FactID)
		merged.Lineage = current.Lineage
		merged.Topic = firstNonEmpty(current.Topic, merged.Topic)
		merged.Visibility = firstNonEmpty(current.Visibility, merged.Visibility)
		merged.Entity, merged.Attribute = k.entity, k.attribute
		byID[current.VersionID] = merged
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]memory.Item, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		snap := f.snapshot(byID[id], retrieved)
		f.seen[id] = Known{FactID: snap.FactID, Entity: snap.Entity, Attribute: snap.Attribute,
			ContentHash: snap.ContentHash, Signer: snap.Signer, Excerpt: snap.Excerpt, ReplacedBy: snap.ReplacedBy}
		items = append(items, SnapshotItem(snap))
	}
	return items, nil
}

func (f *Fetcher) snapshot(v Version, retrieved string) Snapshot {
	s := Snapshot{
		VersionID: v.VersionID, FactID: v.FactID, Entity: v.Entity, Attribute: v.Attribute, Value: v.Value,
		RawStatus: v.Status, ValidFrom: v.ValidFrom, RecordedAt: v.RecordedAt,
		Topic: v.Topic, Visibility: v.Visibility, SourceURI: v.SourceURI, RetrievedAt: retrieved,
	}
	if v.ValidTo != nil {
		s.ValidTo = *v.ValidTo
	}
	if v.RetiredAt != nil {
		s.RetiredAt = *v.RetiredAt
	}
	if v.Lineage != nil {
		s.SourceURI = firstNonEmpty(v.Lineage.SourceURI, s.SourceURI)
		s.ContentHash = v.Lineage.ContentHash
		s.Signer = v.Lineage.Signer
		if v.Lineage.Excerpt != nil {
			s.Excerpt = *v.Lineage.Excerpt
		}
	}
	switch v.Status {
	case "retired":
		s.Status = "replaced"
	case "":
		s.Status = "unknown"
	default:
		s.Status = v.Status
	}
	// Keep what an earlier sync already learned; history rows are thinner.
	if known, ok := f.seen[v.VersionID]; ok {
		s.mergeKnown(known)
	}
	if known, ok := f.lookup(v.VersionID); ok {
		s.mergeKnown(known)
	}
	return s
}

func (s *Snapshot) mergeKnown(k Known) {
	s.FactID = firstNonEmpty(s.FactID, k.FactID)
	s.ContentHash = firstNonEmpty(s.ContentHash, k.ContentHash)
	s.Signer = firstNonEmpty(s.Signer, k.Signer)
	s.Excerpt = firstNonEmpty(s.Excerpt, k.Excerpt)
	if s.Status == "replaced" {
		s.ReplacedBy = firstNonEmpty(s.ReplacedBy, k.ReplacedBy)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// SnapshotItem renders a version as a read-only evidence item with a citation.
func SnapshotItem(s Snapshot) memory.Item {
	title := fmt.Sprintf("%s · %s: %s", s.Entity, s.Attribute, s.Value)
	if s.Status == "replaced" {
		title += " (replaced)"
	}
	if len([]rune(title)) > 200 {
		title = string([]rune(title)[:199]) + "…"
	}
	validTo := s.ValidTo
	if validTo == "" {
		validTo = "open"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Entity: %s\nAttribute: %s\nValue: %s\nStatus: %s\n", s.Entity, s.Attribute, s.Value, s.Status)
	fmt.Fprintf(&b, "Valid: %s → %s\nRecorded: %s\n", orDash(s.ValidFrom), validTo, orDash(s.RecordedAt))
	if s.RetiredAt != "" {
		fmt.Fprintf(&b, "Retired: %s\n", s.RetiredAt)
	}
	if s.ReplacedBy != "" {
		fmt.Fprintf(&b, "Replaced by: %s\n", s.ReplacedBy)
	}
	if s.Topic != "" {
		fmt.Fprintf(&b, "Topic: %s\n", s.Topic)
	}
	fmt.Fprintf(&b, "Citation: %s @ %s\n", orDash(s.SourceURI), s.VersionID)
	if s.ContentHash != "" {
		fmt.Fprintf(&b, "Content hash: %s\n", s.ContentHash)
	}
	if s.Excerpt != "" {
		b.WriteString("\nExcerpt:\n")
		for _, line := range strings.Split(s.Excerpt, "\n") {
			b.WriteString("> " + line + "\n")
		}
	}
	b.WriteString("\nRead-only copy of a governed company memory fact. The service, not Mora, decides its status.\n")
	meta := map[string]any{
		"version_id": s.VersionID, "fact_id": s.FactID, "entity": s.Entity, "attribute": s.Attribute,
		"value": s.Value, "status": s.Status, "eywa_status": s.RawStatus,
		"valid_from": s.ValidFrom, "valid_to": s.ValidTo, "recorded_at": s.RecordedAt, "retired_at": s.RetiredAt,
		"replaced_by": s.ReplacedBy, "topic": s.Topic, "visibility": s.Visibility,
		"source_uri": s.SourceURI, "content_hash": s.ContentHash, "signer": s.Signer, "excerpt": s.Excerpt,
		"occurred_at": s.ValidFrom, "retrieved_at": s.RetrievedAt,
	}
	for k, v := range meta {
		if str, ok := v.(string); ok && str == "" {
			delete(meta, k)
		}
	}
	occurred, err := time.Parse(time.RFC3339, s.RecordedAt)
	if err != nil {
		occurred, _ = time.Parse(time.RFC3339, s.ValidFrom)
	}
	tags := []string{"eywa"}
	if s.Topic != "" {
		tags = append(tags, s.Topic)
	}
	if s.Status == "replaced" {
		tags = append(tags, "replaced")
	}
	return memory.Item{
		Kind: KindFact, ProviderID: s.VersionID, Title: title, Body: b.String(),
		OccurredAt: occurred, Tags: tags, Meta: meta, Payload: s,
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// MapFact maps a pulled item. A RecordFailure item maps to a record carrying
// Meta["eywa_record_error"]; the caller's write step must fail it.
func MapFact(it memory.Item, scope string, budget int) memory.MappedMemory {
	mm := memory.MapItem(it, scope, budget)
	mm.Type = "fact"
	mm.Provider = Provider
	if f, ok := it.Payload.(RecordFailure); ok {
		if mm.Meta == nil {
			mm.Meta = map[string]any{}
		}
		mm.Meta["eywa_record_error"] = fmt.Sprintf("%s: %s", f.Ref, f.Err)
	}
	return mm
}

// KnownFromMeta rebuilds what a vault record knows about its version.
func KnownFromMeta(meta map[string]any) (string, Known, bool) {
	str := func(k string) string {
		s, _ := meta[k].(string)
		return s
	}
	id := str("version_id")
	if id == "" || str("entity") == "" || str("attribute") == "" {
		return "", Known{}, false
	}
	return id, Known{FactID: str("fact_id"), Entity: str("entity"), Attribute: str("attribute"),
		ContentHash: str("content_hash"), Signer: str("signer"), Excerpt: str("excerpt"), ReplacedBy: str("replaced_by")}, true
}

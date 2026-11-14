package mora

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/segments"
)

const watchStateUnavailable = "watch unavailable"

type chatWatch struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	StartedAt string            `json:"started_at"`
	Seen      map[string]string `json:"seen"`
}
type chatWatchStore struct {
	Version int                  `json:"version"`
	VaultID string               `json:"vault_id"`
	Watches map[string]chatWatch `json:"watches"`
}
type chatWatchMessage struct {
	EvidenceRef string `json:"evidence_ref"`
	At          string `json:"at"`
	Sender      string `json:"sender"`
	Direction   string `json:"direction"`
	Change      string `json:"change"`
	Text        string `json:"text"`
	hash        string
}
type chatWatchUpdate struct {
	ID              string             `json:"id"`
	Title           string             `json:"title"`
	State           string             `json:"state"`
	LastSuccessAt   string             `json:"last_success_at,omitempty"`
	Warning         string             `json:"warning,omitempty"`
	CoverageLimited bool               `json:"coverage_limited,omitempty"`
	Messages        []chatWatchMessage `json:"messages"`
	More            int                `json:"more"`
}
type chatWatchReport struct {
	Watches      []chatWatchUpdate `json:"watches"`
	AckRequested bool              `json:"ack_requested"`
}

func chatWatchPath(cfg Config) (string, error) {
	root, err := filepath.EvalSymlinks(cfg.VaultDir)
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg.StateDir, "chat-watches", fmt.Sprintf("%x.json", sha256.Sum256([]byte(root)))), nil
}
func loadChatWatches(cfg Config) (chatWatchStore, string, error) {
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "chat-watches")); errors.Is(err, os.ErrNotExist) {
		return chatWatchStore{Version: 1, Watches: map[string]chatWatch{}}, "", nil
	} else if err != nil {
		return chatWatchStore{}, "", err
	}
	path, err := chatWatchPath(cfg)
	if err != nil {
		return chatWatchStore{}, "", err
	}
	s := chatWatchStore{Version: 1, Watches: map[string]chatWatch{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, path, nil
	}
	if err != nil {
		return s, path, err
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return s, path, fmt.Errorf("read chat watches: %w", err)
	}
	if s.Version != 1 || s.Watches == nil {
		return s, path, errors.New("invalid chat watch state")
	}
	marker, _, err := readVaultMarker(cfg)
	if err != nil {
		return s, path, err
	}
	if s.VaultID != marker.VaultID {
		return s, path, errors.New("chat watches belong to a different vault identity")
	}
	for id, w := range s.Watches {
		if id != w.ID || !strings.HasPrefix(id, "imessage_chat/") || w.Seen == nil {
			return s, path, errors.New("invalid chat watch entry")
		}
	}
	return s, path, nil
}
func saveChatWatches(cfg Config, path string, s chatWatchStore) error {
	marker, _, err := readVaultMarker(cfg)
	if err != nil {
		return err
	}
	s.VaultID = marker.VaultID
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, append(b, '\n'), 0o600)
}
func chatWatchRows(m Memory) ([]segments.Row, error) {
	if m.Provider != "imessage" || !strings.HasPrefix(m.ID, "imessage_chat/") || m.DeletedAt != "" {
		return nil, errors.New("watch requires a current imported iMessage chat")
	}
	rows, diag := segments.Derive(m)
	if diag != nil {
		return nil, fmt.Errorf("message evidence unavailable (%s); sync iMessage again", diag.Reason)
	}
	if _, ok := m.Meta["message_evidence_diagnostics"]; ok {
		return nil, errors.New("message evidence is incomplete; sync iMessage again")
	}
	return rows, nil
}
func chatWatchHash(row segments.Row) string {
	// Byte offsets move when older messages leave the import window; hash content only.
	return fmt.Sprintf("%x", sha256.Sum256([]byte(row.At+"\x00"+row.Sender+"\x00"+segments.Direction(row.BlockRefs)+"\x00"+row.Text)))
}
func chatWatchHealth(cfg Config, now time.Time) sourceHealth {
	for _, h := range sourceHealthAll(cfg, now) {
		if h.Key == "imessage" || strings.HasPrefix(h.Key, "imessage:") {
			return h
		}
	}
	return sourceHealth{Key: "imessage", State: "unavailable"}
}

func chatWatchChanges(w chatWatch, m Memory, health sourceHealth, limit int) chatWatchUpdate {
	u := chatWatchUpdate{ID: w.ID, Title: m.Title, State: health.State, LastSuccessAt: health.LastSuccessAt, Messages: []chatWatchMessage{}, CoverageLimited: m.Truncated}
	rows, err := chatWatchRows(m)
	if err != nil {
		u.State = "unavailable"
		u.Warning = err.Error()
		return u
	}
	if health.State != healthFresh {
		u.Warning = "iMessage sync is not fresh; changes reflect the last imported copy. Run mora sync imessage."
	}
	for _, row := range rows {
		hash := chatWatchHash(row)
		old, seen := w.Seen[row.EvidenceRef]
		if seen && old == hash {
			continue
		}
		change := "new"
		if seen {
			change = "updated"
		}
		u.Messages = append(u.Messages, chatWatchMessage{EvidenceRef: row.EvidenceRef, At: row.At, Sender: row.Sender, Direction: segments.Direction(row.BlockRefs), Change: change, Text: row.Text, hash: hash})
	}
	sort.Slice(u.Messages, func(i, j int) bool {
		a, b := u.Messages[i], u.Messages[j]
		if a.At == b.At {
			return a.EvidenceRef < b.EvidenceRef
		}
		ta, _ := time.Parse(time.RFC3339, a.At)
		tb, _ := time.Parse(time.RFC3339, b.At)
		return ta.Before(tb)
	})
	if len(u.Messages) > limit {
		u.More = len(u.Messages) - limit
		u.Messages = u.Messages[:limit]
	}
	return u
}

func cmdWatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	usage := errors.New("usage: mora watch chats|add <chat-id>|list|check [chat-id] [--ack]|remove <chat-id> [--json]")
	if len(args) == 0 {
		return usage
	}
	if args[0] == "--help" || args[0] == "-h" {
		return flag.ErrHelp
	}
	sub := args[0]
	switch sub {
	case "chats", "add", "list", "check", "remove":
	default:
		return usage
	}
	pos, flags := splitLeadingPositionals(args[1:])
	fs := flag.NewFlagSet("watch "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	var query string
	var ack bool
	limit := 20
	if sub == "chats" {
		fs.StringVar(&query, "query", "", "filter chat titles")
	}
	if sub == "check" {
		fs.BoolVar(&ack, "ack", false, "acknowledge only displayed changes")
		fs.IntVar(&limit, "limit", 20, "messages per chat (1-100)")
	}
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if fs.NArg() != 0 || len(pos) > 1 || limit < 1 || limit > 100 {
		return usage
	}
	id := ""
	if len(pos) == 1 {
		id = pos[0]
	}
	if ((sub == "add" || sub == "remove") && id == "") || ((sub == "chats" || sub == "list") && id != "") {
		return usage
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	emit := func(v any, human string) error {
		if *jsonOut {
			return emitReceipt(stdout, "mora.watch."+sub, 1, v)
		}
		_, err := io.WriteString(stdout, reviewShowText(human))
		return err
	}
	if sub == "chats" {
		ms, err := listMemories(cfg, "", 0)
		if err != nil {
			return err
		}
		chats := []map[string]string{}
		var b strings.Builder
		for _, m := range ms {
			if m.Provider == "imessage" && strings.HasPrefix(m.ID, "imessage_chat/") && strings.Contains(strings.ToLower(m.Title), strings.ToLower(query)) {
				chats = append(chats, map[string]string{"id": m.ID, "title": m.Title})
				fmt.Fprintf(&b, "%s  %s\n", m.ID, strings.Join(strings.Fields(m.Title), " "))
			}
		}
		return emit(map[string]any{"chats": chats}, b.String())
	}
	path, err := chatWatchPath(cfg)
	if err != nil {
		return err
	}
	run := func() error {
		store, _, err := loadChatWatches(cfg)
		if err != nil {
			return err
		}
		switch sub {
		case "add":
			if _, exists := store.Watches[id]; exists {
				return emit(map[string]any{"id": id, "added": false}, "Already watching "+id+"\n")
			}
			m, err := findMemory(cfg, id)
			if err != nil {
				return err
			}
			rows, err := chatWatchRows(m)
			if err != nil {
				return err
			}
			if chatWatchHealth(cfg, time.Now()).State != healthFresh {
				return errors.New("sync iMessage successfully before starting a watch: mora sync imessage")
			}
			w := chatWatch{ID: id, Title: m.Title, StartedAt: time.Now().UTC().Format(time.RFC3339), Seen: map[string]string{}}
			for _, row := range rows {
				w.Seen[row.EvidenceRef] = chatWatchHash(row)
			}
			store.Watches[id] = w
			if err := saveChatWatches(cfg, path, store); err != nil {
				return err
			}
			return emit(map[string]any{"id": id, "added": true, "coverage_limited": m.Truncated}, "Watching "+id+" from its current imported messages. All chats remain imported.\n")
		case "remove":
			_, exists := store.Watches[id]
			delete(store.Watches, id)
			if exists {
				if err := saveChatWatches(cfg, path, store); err != nil {
					return err
				}
			}
			return emit(map[string]any{"id": id, "removed": exists}, "Watch removed (imported messages kept): "+id+"\n")
		case "list":
			entries := []map[string]string{}
			var b strings.Builder
			for _, key := range sortedChatWatchIDs(store) {
				w := store.Watches[key]
				entries = append(entries, map[string]string{"id": w.ID, "title": w.Title, "started_at": w.StartedAt})
				fmt.Fprintf(&b, "%s  %s\n", w.ID, strings.Join(strings.Fields(w.Title), " "))
			}
			return emit(map[string]any{"watches": entries}, b.String())
		case "check":
			if id != "" {
				if _, ok := store.Watches[id]; !ok {
					return errors.New("chat is not watched")
				}
			}
			report := chatWatchReport{Watches: []chatWatchUpdate{}}
			health := chatWatchHealth(cfg, time.Now())
			for _, key := range sortedChatWatchIDs(store) {
				if id != "" && id != key {
					continue
				}
				w := store.Watches[key]
				m, err := findMemory(cfg, key)
				if err != nil {
					report.Watches = append(report.Watches, chatWatchUpdate{ID: key, State: "unavailable", Warning: "Watched chat is missing or no longer visible.", Messages: []chatWatchMessage{}})
					continue
				}
				report.Watches = append(report.Watches, chatWatchChanges(w, m, health, limit))
			}
			if ack {
				for _, u := range report.Watches {
					if u.State != healthFresh {
						return errors.New("cannot acknowledge watch changes while a chat or its sync is unavailable or stale")
					}
				}
			}
			// Persist only after output succeeds; a failed pipe must not consume updates.
			report.AckRequested = ack
			if err := emit(report, renderChatWatchReport(report)); err != nil {
				return err
			}
			if ack {
				for _, u := range report.Watches {
					w := store.Watches[u.ID]
					for _, msg := range u.Messages {
						w.Seen[msg.EvidenceRef] = msg.hash
					}
					store.Watches[u.ID] = w
				}
				if err := saveChatWatches(cfg, path, store); err != nil {
					return fmt.Errorf("changes were displayed but acknowledgement was not saved: %w", err)
				}
				return nil
			}
			return nil
		}
		return usage
	}
	if sub == "add" || sub == "remove" || ack {
		return leasefile.WithGuard(path, run)
	}
	return run()
}
func sortedChatWatchIDs(s chatWatchStore) []string {
	ids := make([]string, 0, len(s.Watches))
	for id := range s.Watches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func renderChatWatchReport(r chatWatchReport) string {
	var b strings.Builder
	if len(r.Watches) == 0 {
		return "No watched chats. Find one with mora watch chats, then mora watch add <chat-id>.\n"
	}
	for _, u := range r.Watches {
		fmt.Fprintf(&b, "%s (%s) — sync: %s; last success: %s\n", strings.Join(strings.Fields(u.Title), " "), u.ID, u.State, u.LastSuccessAt)
		if u.Warning != "" {
			fmt.Fprintln(&b, u.Warning)
		}
		if u.CoverageLimited {
			fmt.Fprintln(&b, "Coverage limited to retained imported messages; missing history is not treated as deletion.")
		}
		for _, m := range u.Messages {
			fmt.Fprintf(&b, "- [%s] %s · %s · %s\n  %s\n  evidence: %s\n", m.Change, m.At, m.Sender, m.Direction, m.Text, m.EvidenceRef)
		}
		if len(u.Messages) == 0 {
			fmt.Fprintln(&b, "No unseen changes in the imported copy.")
		}
		if u.More > 0 {
			fmt.Fprintf(&b, "%d more changes remain.\n", u.More)
		}
	}
	if !r.AckRequested {
		fmt.Fprintln(&b, "Preview only. Use mora watch check --ack to acknowledge displayed changes.")
	}
	return b.String()
}

// Use already-filtered digest inputs so watches cannot bypass scope, source or governance.
func appendChatWatchDigest(cfg Config, now time.Time, d *Digest, byInstance map[string][]Memory, reportMissing bool) error {
	store, _, err := loadChatWatches(cfg)
	section := DigestSection{Source: "imessage:watched", State: stateDelta, Items: []DigestItem{}}
	if err != nil {
		// A damaged optional watch store must not hide the rest of the brief.
		if reportMissing {
			section.State = watchStateUnavailable
			d.Sections = append([]DigestSection{section}, d.Sections...)
		}
		return nil
	}
	if len(store.Watches) == 0 {
		return nil
	}
	health := chatWatchHealth(cfg, now)
	if health.State == healthStale {
		section.State = stateStale
	} else if health.State != healthFresh {
		section.State = watchStateUnavailable
	}
	byID := make(map[string]Memory)
	for _, ms := range byInstance {
		for _, m := range ms {
			byID[m.ID] = m
		}
	}
	for _, key := range sortedChatWatchIDs(store) {
		m, ok := byID[key]
		if !ok {
			if reportMissing {
				section.State = watchStateUnavailable
			}
			continue
		}
		u := chatWatchChanges(store.Watches[key], m, health, 1)
		if len(u.Messages) == 0 && u.Warning == "" {
			continue
		}
		snippet := u.Warning
		if len(u.Messages) > 0 {
			msg := u.Messages[0]
			snippet = fmt.Sprintf("%d pending message changes; %s %s: %s", len(u.Messages)+u.More, msg.At, msg.Change, genericutil.TruncateRunes(strings.Join(strings.Fields(msg.Text), " "), digestSnippetLen))
		}
		if u.Warning != "" && len(u.Messages) > 0 {
			snippet += "; " + u.Warning
		}
		if u.CoverageLimited {
			snippet += "; imported history is truncated"
		}
		snippet += "; mora watch check for messages and citations"
		section.Items = append(section.Items, DigestItem{ID: key, Title: m.Title, Source: "imessage", Snippet: snippet, Lane: "watched", Rationale: "pending until mora watch check --ack"})
	}
	if len(section.Items) > 0 || section.State == watchStateUnavailable {
		d.Sections = append([]DigestSection{section}, d.Sections...)
	}
	return nil
}

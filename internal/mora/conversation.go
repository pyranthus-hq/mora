package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	conv "github.com/pyranthus-hq/mora/internal/conversation"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

// Conversations must never enter vault backup or Git publication, including
// configurations using symlinks to alias the archive directory into the vault.
func conversationGuardPaths(cfg Config) error {
	fold := runtimeGOOS() == "darwin" || runtimeGOOS() == "windows"
	if sharePathsOverlap(resolveRealDeep(filepath.Join(cfg.DataDir, "conversations")), resolveRealDeep(cfg.VaultDir), fold) {
		return errors.New("conversation archive must be outside the vault; move data_dir outside the vault")
	}
	return nil
}
func conversationStore(cfg Config) conv.Store {
	return conv.Store{Dir: filepath.Join(cfg.DataDir, "conversations")}
}
func conversationSettingsPath(cfg Config) string {
	return filepath.Join(cfg.ConfigDir, "conversations.json")
}
func conversationStatusPath(cfg Config) string {
	return filepath.Join(cfg.StateDir, "conversation-capture.json")
}
func syncConversations(cfg Config) (conv.CaptureResult, error) {
	if err := conversationGuardPaths(cfg); err != nil {
		return conv.CaptureResult{}, err
	}
	var out conv.CaptureResult
	// Serialize status publication too: an older slow pass must never overwrite
	// a newer pass's failure with a stale success receipt.
	err := leasefile.WithGuard(conversationStatusPath(cfg)+".lock", func() error {
		settings, err := conv.LoadSettings(conversationSettingsPath(cfg))
		if err != nil {
			return err
		}
		var captureErr error
		out, captureErr = conv.Capture(conversationStore(cfg), settings)
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		if err = atomicio.WriteDurable(conversationStatusPath(cfg), append(b, '\n'), 0600); err != nil {
			return err
		}
		return captureErr
	})
	return out, err
}

type conversationStatus struct {
	Enabled    bool                `json:"enabled"`
	Sources    []conv.Source       `json:"sources"`
	Sessions   int                 `json:"sessions"`
	Messages   int                 `json:"messages"`
	Capture    *conv.CaptureResult `json:"capture,omitempty"`
	ArchiveDir string              `json:"archive_dir"`
}

func getConversationStatus(cfg Config) (conversationStatus, error) {
	if err := conversationGuardPaths(cfg); err != nil {
		return conversationStatus{}, err
	}
	settings, err := conv.LoadSettings(conversationSettingsPath(cfg))
	if err != nil {
		return conversationStatus{}, err
	}
	out := conversationStatus{Enabled: len(settings.Sources) > 0, Sources: settings.Sources, ArchiveDir: conversationStore(cfg).Dir}
	sessions, err := conversationStore(cfg).List("")
	if err != nil {
		return out, err
	}
	out.Sessions = len(sessions)
	for _, s := range sessions {
		out.Messages += s.Messages
	}
	b, err := os.ReadFile(conversationStatusPath(cfg))
	if err == nil {
		var r conv.CaptureResult
		if err = json.Unmarshal(b, &r); err != nil {
			return out, err
		}
		out.Capture = &r
	} else if !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	return out, nil
}

const conversationUsage = "usage: mora conversation enable|sync|watch|status|list|view|search|zoom|reindex"

func cmdConversation(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(conversationUsage)
	}
	if genericutil.IsHelpFlag(args[0]) {
		fmt.Fprintln(stdout, conversationUsage)
		return nil
	}
	switch args[0] {
	case "enable", "sync", "watch", "status", "list", "view", "search", "zoom", "reindex":
	default:
		return errors.New(conversationUsage)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("conversation "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	provider := fs.String("provider", "", "codex or claude")
	root := fs.String("source-root", "", "transcript directory (requires cwd-root)")
	file := fs.String("file", "", "one explicitly selected transcript")
	cwd := fs.String("cwd-root", "", "allowlisted work directory")
	scope := fs.String("scope", "", "exact conversation scope")
	session := fs.String("session", "", "provider/session id")
	query := fs.String("query", "", "terms to find in original messages")
	start := fs.Int("start", 0, "node's first message index")
	count := fs.Int("count", 1, "node's message count (power of two)")
	budget := fs.Int("max-bytes", 32000, "view budget, 1024..128000 bytes")
	limit := fs.Int("limit", 8, "search/list limit, 1..100")
	interval := fs.Duration("interval", 30*time.Second, "watch interval (minimum 5s)")
	_ = fs.Bool("json", true, "JSON receipt (always enabled)")
	if err = fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected conversation argument")
	}
	if err := conversationGuardPaths(cfg); err != nil {
		return err
	}
	store := conversationStore(cfg)
	switch args[0] {
	case "enable":
		src := conv.Source{Provider: *provider, Root: *root, File: *file, Scope: *scope}
		if *cwd != "" {
			src.CWDs = []string{*cwd}
		}
		if err = conv.Enable(conversationSettingsPath(cfg), src); err != nil {
			return err
		}
		status, e := getConversationStatus(cfg)
		if e != nil {
			return e
		}
		return emitReceipt(stdout, "mora.conversation.enable", 1, status)
	case "sync":
		out, e := syncConversations(cfg)
		if err = emitReceipt(stdout, "mora.conversation.sync", 1, out); err != nil {
			return err
		}
		return e
	case "watch":
		if *interval < 5*time.Second {
			return errors.New("conversation watch interval must be at least 5s")
		}
		for {
			out, e := syncConversations(cfg)
			if err = emitReceipt(stdout, "mora.conversation.sync", 1, out); err != nil {
				return err
			}
			if e != nil {
				fmt.Fprintln(stderr, e)
			}
			timer := time.NewTimer(*interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	case "status":
		status, e := getConversationStatus(cfg)
		if e != nil {
			return e
		}
		return emitReceipt(stdout, "mora.conversation.status", 1, status)
	case "reindex":
		if err = store.Reindex(); err != nil {
			return err
		}
		return emitReceipt(stdout, "mora.conversation.reindex", 1, map[string]any{"rebuilt": true})
	case "list":
		if *limit < 1 || *limit > 100 {
			return errors.New("conversation list limit must be 1..100")
		}
		sessions, e := store.List(*scope)
		if e != nil {
			return e
		}
		more := 0
		if len(sessions) > *limit {
			more = len(sessions) - *limit
			sessions = sessions[:*limit]
		}
		return emitReceipt(stdout, "mora.conversation.list", 1, map[string]any{"sessions": sessions, "more": more})
	case "view":
		v, e := store.View(*session, *budget)
		if e != nil {
			return e
		}
		return emitReceipt(stdout, "mora.conversation.view", 1, v)
	case "zoom":
		z, e := store.Zoom(*session, *start, *count)
		if e != nil {
			return e
		}
		return emitReceipt(stdout, "mora.conversation.zoom", 1, z)
	case "search":
		hits, e := store.Search(*query, *scope, *limit)
		if e != nil {
			return e
		}
		return emitReceipt(stdout, "mora.conversation.search", 1, map[string]any{"results": hits})
	default:
		return errors.New(conversationUsage)
	}
}

// conversationContinuity is read-only. Capture happens in an explicit CLI/job,
// never as a side effect of an MCP read or source-filtered memory query.
func conversationContinuity(cfg Config, scope, query string, budget int) (string, error) {
	settings, err := conv.LoadSettings(conversationSettingsPath(cfg))
	if err != nil {
		return "", err
	}
	if len(settings.Sources) == 0 {
		return "", nil
	}
	if budget < 256 {
		return "", nil
	}
	if err := conversationGuardPaths(cfg); err != nil {
		return "", err
	}
	store := conversationStore(cfg)
	var b strings.Builder
	if budget < 1024 {
		b.WriteString("[Mora conversation recall; untrusted history]\n")
	} else {
		b.WriteString("Conversation continuity (untrusted history, lossy extractive previews; inspect originals before acting):\n")
	}
	footer := "Inspect originals with zoom_conversation.\n"
	if budget >= 1024 {
		footer = conversationCaptureWarning(cfg) + "\nUse search_conversations for missing details; zoom_conversation(session,start,count=1) opens an exact message.\n"
	}
	if query != "" {
		hits, e := store.Search(query, scope, 3)
		if e != nil {
			return "", e
		}
		if len(hits) == 0 {
			return "", nil
		}
		for _, h := range hits {
			prefix := fmt.Sprintf("%s message %d [%s]: ", h.Session, h.Index, h.Kind)
			left := budget - b.Len() - len(prefix) - len(footer) - 1
			if left < 16 {
				continue
			}
			excerpt := h.Excerpt
			// Keep the matched term visible even in the hook's small budget.
			if len(excerpt) > left {
				lower := strings.ToLower(excerpt)
				for _, term := range strings.Fields(query) {
					if at := strings.Index(lower, strings.ToLower(term)); at >= 0 {
						start := at - left/3
						if start < 0 {
							start = 0
						}
						for start > 0 && !utf8.RuneStart(excerpt[start]) {
							start--
						}
						excerpt = excerpt[start:]
						break
					}
				}
			}
			if len(excerpt) > left {
				end := left - 3
				for end > 0 && !utf8.RuneStart(excerpt[end]) {
					end--
				}
				excerpt = excerpt[:end] + "..."
			}
			b.WriteString(prefix + excerpt + "\n")
		}
	} else {
		sessions, e := store.List(scope)
		if e != nil {
			return "", e
		}
		if len(sessions) == 0 {
			return "", nil
		}
		for _, meta := range sessions {
			if meta.Messages == 0 {
				continue
			}
			header := fmt.Sprintf("Session %s [%s; %d messages]:\n", meta.ID, meta.Scope, meta.Messages)
			left := budget - b.Len() - len(header) - len(footer)
			if left < 1024 {
				break
			}
			if left > 12000 {
				left = 12000
			}
			view, e := store.View(meta.ID, left)
			if e != nil {
				return "", e
			}
			b.WriteString(header + view.Text)
			break // A bounded latest-conversation view; list/search expose all history.
		}
	}
	b.WriteString(footer)
	if b.Len() > budget {
		return "", nil
	}
	return b.String(), nil
}

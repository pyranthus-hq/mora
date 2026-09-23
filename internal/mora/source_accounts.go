package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// Account verification checks the credential's live provider identity, not the
// configured label or recipients found in a stored message. It never reconnects
// accounts, changes source bindings or claims mailbox completeness.
type sourceAccountCheck struct {
	Name          string `json:"name"`
	Account       string `json:"account"`
	ExpectedEmail string `json:"expected_email,omitempty"`
	ActualEmail   string `json:"actual_email,omitempty"`
	State         string `json:"state"`
	Issue         string `json:"issue,omitempty"`
}

func accountEmail(value string) string {
	a, err := mail.ParseAddress(strings.TrimSpace(value))
	if err != nil || a.Address != strings.TrimSpace(value) {
		return ""
	}
	return strings.ToLower(a.Address)
}

func requireExpectedGoogleAccount(expected, actual string) error {
	if expected == "" {
		return nil
	} // Legacy unbound sources need explicit verification.
	if accountEmail(expected) == "" || accountEmail(actual) == "" || accountEmail(expected) != accountEmail(actual) {
		return fmt.Errorf("Google mailbox identity does not match this source's configured email; verify accounts with mora sources verify --json and reconnect the intended account before syncing")
	}
	return nil
}

// verifyGoogleSyncIdentity is shared by Gmail and Calendar. A calendar-only
// sync still uses the Gmail profile endpoint because one OAuth credential owns
// both APIs; no source can advance a cursor without a verified mailbox.
func verifyGoogleSyncIdentity(s Source, kind google.ItemKind, actual string) error {
	if kind != google.KindGmailThread && kind != google.KindCalEvent {
		return fmt.Errorf("unsupported Google sync kind %q", kind)
	}
	if accountEmail(actual) == "" {
		return errors.New("Google mailbox identity could not be verified before sync")
	}
	return requireExpectedGoogleAccount(s.Email, actual)
}

func bindGoogleSyncStatus(st *memory.SyncStatus, actual string) bool {
	if st.AccountEmail == actual {
		return false
	}
	// Older binaries persist this file without account_email. Binding the
	// verified mailbox onto that record is not an identity change — wiping
	// IncrementalCursor here sent personal Gmail back through a 90-day walk.
	if st.AccountEmail == "" {
		// An empty identity does not PROVE this state came from `actual`, but
		// the state file is per source instance and a cursor that did not come
		// from this mailbox self-heals: Gmail rejects a foreign history id,
		// ingest maps that to ErrIncrementalCursorExpired, and the run falls
		// back to a full snapshot. One wasted call is the whole downside, and
		// it is cheaper than sending every legacy record through a 90-day walk.
		st.AccountEmail = actual
		return true
	}
	st.AccountEmail = actual
	st.Checkpoint = ""
	st.CursorMode = ""
	st.IncrementalCursor = ""
	// A staged snapshot baseline belongs to the mailbox that issued it. Leaving
	// it behind would let the next completed walk promote another account's
	// history id into this record's incremental cursor.
	st.PendingSyncCursor = ""
	st.GmailHistory = ""
	st.CalSyncToken = ""
	st.LastSuccessAt = ""
	st.LastSynced = ""
	st.LastError = "Mailbox identity changed or was not bound; a new snapshot must finish before this source is fresh."
	return true
}

func checkSourceAccounts(ctx context.Context, sources []Source, lookup func(context.Context, string) (string, error)) []sourceAccountCheck {
	checks := []sourceAccountCheck{}
	for _, s := range sources {
		if s.Type != "gmail" {
			continue
		}
		row := sourceAccountCheck{Name: s.Name, Account: s.Account, ExpectedEmail: accountEmail(s.Email), State: "disabled"}
		if s.IsEnabled() {
			value, err := lookup(ctx, s.Account)
			row.ActualEmail = accountEmail(value)
			switch {
			case err != nil || row.ActualEmail == "":
				row.State = "unavailable"
				row.Issue = "The connected Google account could not be verified."
			case row.ExpectedEmail != "" && row.ExpectedEmail != row.ActualEmail:
				row.State = "mismatch"
				row.Issue = "The credential belongs to a different mailbox than this source declares."
			case row.ExpectedEmail == "":
				row.State = "unbound"
				row.Issue = "Live mailbox verified, but this source has no expected email binding."
			default:
				row.State = "verified"
			}
		}
		checks = append(checks, row)
	}
	for i := range checks {
		if checks[i].ActualEmail == "" {
			continue
		}
		for j := range checks {
			if i != j && checks[i].ActualEmail == checks[j].ActualEmail && checks[i].Account != checks[j].Account {
				checks[i].State = "duplicate-account"
				checks[i].Issue = "Different source labels resolve to the same live mailbox; they do not provide separate account coverage."
			}
		}
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	return checks
}

func liveGoogleAccount(ctx context.Context, cfg Config, account string) (string, error) {
	oc, err := google.ResolveOAuthConfig(google.Scopes)
	if err != nil {
		return "", err
	}
	tok, err := google.LoadToken(googleTokenPathFor(cfg, account))
	if err != nil {
		return "", err
	}
	f, err := google.NewLiveFetcher(ctx, oc, tok)
	if err != nil {
		return "", err
	}
	return f.AuthedEmailContext(ctx)
}

func verifySourceAccounts(ctx context.Context, cfg Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sources verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: mora sources verify --json")
	}
	sources, err := loadSources(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	checks := checkSourceAccounts(ctx, sources, func(ctx context.Context, account string) (string, error) { return liveGoogleAccount(ctx, cfg, account) })
	return emitReceipt(out, "mora.sources.verify", 1, struct {
		Accounts []sourceAccountCheck `json:"accounts"`
		Coverage string               `json:"coverage"`
	}{checks, "Live Google mailbox identity only. Message coverage, aliases, freshness and other providers are not verified."})
}

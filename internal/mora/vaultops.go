package mora

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/vaultarchive"
)

func cmdLint(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if parseErr := fs.Parse(args); parseErr != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", parseErr, "%v", parseErr)
	}
	if fs.NArg() != 0 {
		return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	// lint-weekly producer chokepoint (HEALTH-11).
	stampOutput := stdout
	if *jsonOut {
		stampOutput = stderr
	}
	defer stampChokepoint(cfg, stampOutput, args, "lint-weekly", producerClock(), &err)
	required := []string{"index.md", "priority-map.md", "live-tasks.md", "heartbeat.md", "auto-resolver.md", "meetings/ledger.md", "log.md"}
	issues := make([]string, 0)
	for _, rel := range required {
		if _, err := os.Stat(filepath.Join(cfg.VaultDir, rel)); err != nil {
			issues = append(issues, "missing "+rel)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "tokens")); err == nil {
		if strings.HasPrefix(filepath.Join(cfg.ConfigDir, "tokens"), cfg.VaultDir) {
			issues = append(issues, "tokens directory is inside vault")
		}
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.lint.report", 1, struct {
			OK     bool     `json:"ok"`
			Issues []string `json:"issues"`
		}{OK: len(issues) == 0, Issues: issues})
	}
	if len(issues) == 0 {
		fmt.Fprintln(stdout, "lint ok")
		return nil
	}
	for _, issue := range issues {
		fmt.Fprintln(stdout, issue)
	}
	return nil
}
func cmdBackup(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if parseErr := fs.Parse(args); parseErr != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", parseErr, "%v", parseErr)
	}
	if fs.NArg() != 0 {
		return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	// backup-daily producer chokepoint (HEALTH-11).
	stampOutput := stdout
	if *jsonOut {
		stampOutput = stderr
	}
	now := producerClock()
	defer stampChokepoint(cfg, stampOutput, args, "backup-daily", now, &err)
	// A drifted config that nests data_dir/config inside the vault would tar the
	// age share identity and DECRYPTED share corpora (plus the index's decrypted
	// text) straight into the backup archive. Refuse rather than leak — the same
	// containment the share verbs and doctor's share_disjoint_from_vault check
	// enforce. Fix the layout (data_dir/config outside the vault), then re-run.
	if err := shareGuardPaths(cfg); err != nil {
		return fmt.Errorf("refusing to back up: %w — Fix the layout (data_dir/config outside the vault), then re-run `mora backup`", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "backups"), 0o700); err != nil {
		return err
	}
	out := filepath.Join(cfg.StateDir, "backups", "mora-"+now.Format("20060102-150405")+".tar.gz")
	if err := tarGz(out, cfg.VaultDir); err != nil {
		return err
	}
	if *jsonOut {
		info, statErr := os.Stat(out)
		if statErr != nil {
			return statErr
		}
		return emitReceipt(stdout, "mora.backup", 1, struct {
			ArchivePath string `json:"archive_path"`
			Bytes       int64  `json:"bytes"`
			CreatedAt   string `json:"created_at"`
		}{ArchivePath: out, Bytes: info.Size(), CreatedAt: now.UTC().Format(time.RFC3339)})
	}
	fmt.Fprintf(stdout, "%s\n", out)
	return nil
}
func tarGz(out, root string) error { return vaultarchive.Write(out, root) }

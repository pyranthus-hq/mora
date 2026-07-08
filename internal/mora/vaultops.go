package mora

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func cmdLint(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	required := []string{"index.md", "priority-map.md", "live-tasks.md", "heartbeat.md", "auto-resolver.md", "meetings/ledger.md", "log.md"}
	var issues []string
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
	if len(issues) == 0 {
		fmt.Fprintln(stdout, "lint ok")
		return nil
	}
	for _, issue := range issues {
		fmt.Fprintln(stdout, issue)
	}
	return nil
}
func cmdBackup(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// A drifted config that nests data_dir/config inside the vault would tar the
	// age share identity and DECRYPTED share corpora (plus the index's decrypted
	// text) straight into the backup archive. Refuse rather than leak — the same
	// containment the share verbs and doctor's share_disjoint_from_vault check
	// enforce. Fix the layout (data_dir/config outside the vault), then re-run.
	if err := shareGuardPaths(cfg); err != nil {
		return fmt.Errorf("refusing to back up: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "backups"), 0o700); err != nil {
		return err
	}
	out := filepath.Join(cfg.StateDir, "backups", "mora-"+time.Now().Format("20060102-150405")+".tar.gz")
	if err := tarGz(out, cfg.VaultDir); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\n", out)
	return nil
}
func tarGz(out, root string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Dir(root), path)
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
}

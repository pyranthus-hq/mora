package mora

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	cfg, err := loadConfig()
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
func cmdBackup(ctx context.Context, args []string, stdout io.Writer) (err error) {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// backup-daily producer chokepoint (HEALTH-11).
	defer stampChokepoint(cfg, stdout, args, "backup-daily", producerClock(), &err)
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
	out := filepath.Join(cfg.StateDir, "backups", "mora-"+time.Now().Format("20060102-150405")+".tar.gz")
	if err := tarGz(out, cfg.VaultDir); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\n", out)
	return nil
}
func tarGz(out, root string) error {
	return tarGzWithOps(out, root, defaultTarGzOps())
}

type tarArchiveWriter interface {
	io.Writer
	WriteHeader(*tar.Header) error
	Close() error
}

type backupArchiveFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
}

type tarGzOps struct {
	createTemp func(string, string) (backupArchiveFile, error)
	walk       func(string, filepath.WalkFunc) error
	open       func(string) (io.ReadCloser, error)
	newGzip    func(io.Writer) io.WriteCloser
	newTar     func(io.Writer) tarArchiveWriter
	rename     func(string, string) error
	remove     func(string) error
}

func defaultTarGzOps() tarGzOps {
	return tarGzOps{
		createTemp: func(dir, pattern string) (backupArchiveFile, error) {
			return os.CreateTemp(dir, pattern)
		},
		walk: filepath.Walk,
		open: func(path string) (io.ReadCloser, error) {
			return os.Open(path)
		},
		newGzip: func(w io.Writer) io.WriteCloser {
			return gzip.NewWriter(w)
		},
		newTar: func(w io.Writer) tarArchiveWriter {
			return tar.NewWriter(w)
		},
		rename: os.Rename,
		remove: os.Remove,
	}
}

func tarGzWithOps(out, root string, ops tarGzOps) (retErr error) {
	f, err := ops.createTemp(filepath.Dir(out), "."+filepath.Base(out)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := f.Name()
	published := false
	defer func() {
		if published {
			return
		}
		if err := ops.remove(tempPath); err != nil && !os.IsNotExist(err) {
			retErr = errors.Join(retErr, fmt.Errorf("remove incomplete backup %q: %w", tempPath, err))
		}
	}()

	gz := ops.newGzip(f)
	tw := ops.newTar(gz)
	walkErr := ops.walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(filepath.Dir(root), path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		in, err := ops.open(path)
		if err != nil {
			return fmt.Errorf("open %q: %w", path, err)
		}
		_, copyErr := io.Copy(tw, in)
		closeErr := in.Close()
		if copyErr != nil {
			copyErr = fmt.Errorf("read %q: %w", path, copyErr)
		}
		if closeErr != nil {
			closeErr = fmt.Errorf("close %q: %w", path, closeErr)
		}
		return errors.Join(copyErr, closeErr)
	})

	finalizeErr := errors.Join(
		wrapBackupFinalizeError("walk vault", walkErr),
		wrapBackupFinalizeError("close tar writer", tw.Close()),
		wrapBackupFinalizeError("close gzip writer", gz.Close()),
		wrapBackupFinalizeError("sync archive", f.Sync()),
		wrapBackupFinalizeError("close archive", f.Close()),
	)
	if finalizeErr != nil {
		return fmt.Errorf("create backup archive: %w", finalizeErr)
	}
	if err := ops.rename(tempPath, out); err != nil {
		return fmt.Errorf("publish backup archive: %w", err)
	}
	published = true
	return nil
}

func wrapBackupFinalizeError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

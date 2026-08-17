// Package vaultarchive owns fail-closed, crash-safe tar.gz vault snapshots.
package vaultarchive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Write creates and atomically publishes a portable tar.gz archive.
func Write(out, root string) error {
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

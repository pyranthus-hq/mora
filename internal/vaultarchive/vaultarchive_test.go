package vaultarchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestTarGzFailsClosedOnWalkError(t *testing.T) {
	root, out := backupTestPaths(t)
	wantErr := errors.New("walk failed")
	ops := defaultTarGzOps()
	ops.walk = func(path string, fn filepath.WalkFunc) error {
		return fn(filepath.Join(path, "unreadable"), nil, wantErr)
	}

	err := tarGzWithOps(out, root, ops)
	if !errors.Is(err, wantErr) {
		t.Fatalf("tarGzWithOps error = %v, want walk error", err)
	}
	assertNoBackupArtifacts(t, out)
}

func TestTarGzUsesPortableHeaderNames(t *testing.T) {
	root, out := backupTestPaths(t)
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "detail.md"), []byte("detail"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(out, root); err != nil {
		t.Fatalf("tarGz: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}

	want := "vault/nested/detail.md"
	for _, name := range names {
		if name == want {
			return
		}
	}
	t.Fatalf("raw tar header names = %q, want portable nested path %q", names, want)
}

func TestTarGzFailsClosedOnSourceReadError(t *testing.T) {
	root, out := backupTestPaths(t)
	wantErr := errors.New("read failed")
	ops := defaultTarGzOps()
	ops.open = func(string) (io.ReadCloser, error) {
		return io.NopCloser(errorReader{err: wantErr}), nil
	}

	err := tarGzWithOps(out, root, ops)
	if !errors.Is(err, wantErr) {
		t.Fatalf("tarGzWithOps error = %v, want read error", err)
	}
	assertNoBackupArtifacts(t, out)
}

func TestTarGzFailsClosedOnSourceCloseError(t *testing.T) {
	root, out := backupTestPaths(t)
	wantErr := errors.New("source close failed")
	ops := defaultTarGzOps()
	ops.open = func(path string) (io.ReadCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &closeErrorReader{ReadCloser: f, err: wantErr}, nil
	}

	err := tarGzWithOps(out, root, ops)
	if !errors.Is(err, wantErr) {
		t.Fatalf("tarGzWithOps error = %v, want source close error", err)
	}
	assertNoBackupArtifacts(t, out)
}

func TestTarGzFailsClosedWhenTarFinalizationFails(t *testing.T) {
	root, out := backupTestPaths(t)
	wantErr := errors.New("tar close failed")
	if err := os.WriteFile(out, []byte("previous valid backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := defaultTarGzOps()
	ops.newTar = func(w io.Writer) tarArchiveWriter {
		return &closeErrorTarWriter{Writer: tar.NewWriter(w), err: wantErr}
	}

	err := tarGzWithOps(out, root, ops)
	if !errors.Is(err, wantErr) {
		t.Fatalf("tarGzWithOps error = %v, want tar close error", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read existing backup: %v", err)
	}
	if string(got) != "previous valid backup" {
		t.Fatalf("existing backup was changed to %q", got)
	}
	assertNoTemporaryBackup(t, out)
}

func TestTarGzFailsClosedWhenGzipFinalizationFails(t *testing.T) {
	root, out := backupTestPaths(t)
	wantErr := errors.New("gzip close failed")
	ops := defaultTarGzOps()
	ops.newGzip = func(w io.Writer) io.WriteCloser {
		return &closeErrorWriteCloser{WriteCloser: gzip.NewWriter(w), err: wantErr}
	}

	err := tarGzWithOps(out, root, ops)
	if !errors.Is(err, wantErr) {
		t.Fatalf("tarGzWithOps error = %v, want gzip close error", err)
	}
	assertNoBackupArtifacts(t, out)
}

func TestTarGzFailsClosedWhenArchiveCloseFails(t *testing.T) {
	root, out := backupTestPaths(t)
	wantErr := errors.New("archive close failed")
	ops := defaultTarGzOps()
	ops.createTemp = func(dir, pattern string) (backupArchiveFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return &closeErrorArchiveFile{File: f, err: wantErr}, nil
	}

	err := tarGzWithOps(out, root, ops)
	if !errors.Is(err, wantErr) {
		t.Fatalf("tarGzWithOps error = %v, want archive close error", err)
	}
	assertNoBackupArtifacts(t, out)
}

func backupTestPaths(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "vault")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "memory.md"), []byte("durable memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(base, "backup.tar.gz")
}

func assertNoBackupArtifacts(t *testing.T, out string) {
	t.Helper()
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("failed backup left final archive: %v", err)
	}
	assertNoTemporaryBackup(t, out)
}

func assertNoTemporaryBackup(t *testing.T, out string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(out), "."+filepath.Base(out)+".tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("failed backup left temporary archives: %v", matches)
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

type closeErrorReader struct {
	io.ReadCloser
	err error
}

type closeErrorWriteCloser struct {
	io.WriteCloser
	err error
}

func (w *closeErrorWriteCloser) Close() error {
	return errors.Join(w.WriteCloser.Close(), w.err)
}

func (r *closeErrorReader) Close() error {
	return errors.Join(r.ReadCloser.Close(), r.err)
}

type closeErrorTarWriter struct {
	Writer *tar.Writer
	err    error
}

func (w *closeErrorTarWriter) Write(p []byte) (int, error) {
	return w.Writer.Write(p)
}

func (w *closeErrorTarWriter) WriteHeader(hdr *tar.Header) error {
	return w.Writer.WriteHeader(hdr)
}

func (w *closeErrorTarWriter) Close() error {
	return errors.Join(w.Writer.Close(), w.err)
}

type closeErrorArchiveFile struct {
	*os.File
	err error
}

func (f *closeErrorArchiveFile) Close() error {
	return errors.Join(f.File.Close(), f.err)
}

func TestCoreB_UtilTarGz(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("AAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("BBB"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(base, "out.tar.gz")
	if err := Write(out, root); err != nil {
		t.Fatalf("tarGz: %v", err)
	}

	entries := coreBUtilReadTarGz(t, out)
	if entries["root/a.txt"] != "AAA" {
		t.Fatalf("root/a.txt = %q, want AAA (entries: %v)", entries["root/a.txt"], entries)
	}
	if entries["root/sub/b.txt"] != "BBB" {
		t.Fatalf("root/sub/b.txt = %q, want BBB (entries: %v)", entries["root/sub/b.txt"], entries)
	}
	// Directories are skipped (info.IsDir short-circuit) — only files recorded.
	if len(entries) != 2 {
		t.Fatalf("expected exactly 2 file entries, got %d: %v", len(entries), entries)
	}

	// os.Create error: output in a nonexistent directory.
	if err := Write(filepath.Join(base, "nope", "deep", "x.tar.gz"), root); err == nil {
		t.Fatal("tarGz to a path in a nonexistent dir must error")
	}
}

func coreBUtilReadTarGz(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if _, err := io.Copy(&b, tr); err != nil {
			t.Fatal(err)
		}
		out[filepath.ToSlash(hdr.Name)] = b.String()
	}
	return out
}

type headerErrorTarWriter struct {
	writer *tar.Writer
	err    error
}

func (w *headerErrorTarWriter) Write(p []byte) (int, error)   { return w.writer.Write(p) }
func (w *headerErrorTarWriter) WriteHeader(*tar.Header) error { return w.err }
func (w *headerErrorTarWriter) Close() error                  { return w.writer.Close() }

func TestWriteFailsClosedOnSourceOpenError(t *testing.T) {
	root, out := backupTestPaths(t)
	want := errors.New("open failed")
	ops := defaultTarGzOps()
	ops.open = func(string) (io.ReadCloser, error) { return nil, want }
	if err := tarGzWithOps(out, root, ops); !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	assertNoBackupArtifacts(t, out)
}

func TestWriteFailsClosedOnHeaderError(t *testing.T) {
	root, out := backupTestPaths(t)
	want := errors.New("header failed")
	ops := defaultTarGzOps()
	ops.newTar = func(w io.Writer) tarArchiveWriter { return &headerErrorTarWriter{writer: tar.NewWriter(w), err: want} }
	if err := tarGzWithOps(out, root, ops); !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	assertNoBackupArtifacts(t, out)
}

func TestWriteFailsClosedOnPublishAndCleanupErrors(t *testing.T) {
	root, out := backupTestPaths(t)
	publishErr, cleanupErr := errors.New("rename failed"), errors.New("cleanup failed")
	ops := defaultTarGzOps()
	ops.rename = func(string, string) error { return publishErr }
	ops.remove = func(string) error { return cleanupErr }
	err := tarGzWithOps(out, root, ops)
	if !errors.Is(err, publishErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("err=%v", err)
	}
}

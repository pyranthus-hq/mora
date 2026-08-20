package sharing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/storage"
)

func shareSubRoot(cfg Config, name string) string { return SubscriptionRoot(cfg.DataDir, name) }

func testStorageRoots(cfg Config) storage.Roots {
	return storage.Roots{VaultDir: cfg.VaultDir, ConfigDir: cfg.ConfigDir, DataDir: cfg.DataDir, StateDir: cfg.StateDir}
}

func TestStorageAdmissionIsWholeProduct(t *testing.T) {
	cfg := packetHConfig(t)
	base, err := storage.ProductBytes(testStorageRoots(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		p := filepath.Join(shareSubRoot(cfg, name), "repo", "pack")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte{'x'}, 70), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := NewStorageAdmissionWithLimit(testStorageRoots(cfg), "gamma", base+120)
	if err := a.CheckCurrent(); err == nil {
		t.Fatal("two subscriptions were admitted as independent footprints")
	}
	if err := os.RemoveAll(shareSubRoot(cfg, "beta")); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckCurrent(); err != nil {
		t.Fatalf("one 70-byte subscription should fit the 120-byte aggregate headroom: %v", err)
	}
}

func TestShareStorageLimitIncludesAllProductRoots(t *testing.T) {
	cfg := packetHConfig(t)
	sizes := []int{11, 12, 13, 14}
	for i, root := range []string{cfg.VaultDir, cfg.ConfigDir, cfg.DataDir, cfg.StateDir} {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("root-%d", i)), bytes.Repeat([]byte{'x'}, sizes[i]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := storage.ProductBytes(testStorageRoots(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got != 50 {
		t.Fatalf("whole-product roots counted %d bytes; want 50", got)
	}

	t.Run("canonical overlap and hard links are deduplicated", func(t *testing.T) {
		root := t.TempDir()
		overlap := Config{
			DataDir:   root,
			VaultDir:  filepath.Join(root, "vault"),
			ConfigDir: filepath.Join(root, "config"),
			StateDir:  filepath.Join(root, "state"),
		}
		for _, dir := range []string{overlap.VaultDir, overlap.ConfigDir, overlap.StateDir} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		src := filepath.Join(overlap.VaultDir, "body")
		if err := os.WriteFile(src, bytes.Repeat([]byte{'h'}, 31), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(src, filepath.Join(overlap.StateDir, "same-body")); err != nil {
			t.Skipf("hard links unavailable on test filesystem: %v", err)
		}
		got, err := storage.ProductBytes(testStorageRoots(overlap))
		if err != nil {
			t.Fatal(err)
		}
		if got != 31 {
			t.Fatalf("nested roots/hard link charged %d bytes; want one 31-byte identity", got)
		}
	})
}

func TestShareStorageLimitIncludesRepo(t *testing.T) {
	cfg := packetHConfig(t)
	base, _ := storage.ProductBytes(testStorageRoots(cfg))
	p := filepath.Join(shareRepoDir(cfg, "acme"), "objects", "pack", "pack-test")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte{'p'}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewStorageAdmissionWithLimit(testStorageRoots(cfg), "acme", base+4095)
	if err := a.CheckCurrent(); err == nil {
		t.Fatal("repo pack bytes were omitted from admission")
	}
}

func TestShareStorageLimitIncludesFetchStaging(t *testing.T) {
	cfg := packetHConfig(t)
	base, _ := storage.ProductBytes(testStorageRoots(cfg))
	p := filepath.Join(shareFetchDir(cfg, "acme", "run"), "object")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte{'f'}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewStorageAdmissionWithLimit(testStorageRoots(cfg), "acme", base+4095)
	if err := a.CheckCurrent(); err == nil {
		t.Fatal("fetch staging bytes were omitted from admission")
	}
}

func TestLegalLargeShareIsNotHardCappedAt4GiB(t *testing.T) {
	cfg := packetHConfig(t)
	legalCorpusBytes := int64(MaxShareEntries) * int64(MaxMemoryBytes)
	if legalCorpusBytes <= 4<<30 {
		t.Fatalf("fixture is not larger than the retired 4 GiB cap: %d", legalCorpusBytes)
	}
	// The opt-in remains proportional to the protocol-legal corpus, not a fixed
	// product cap. Nine corpus-widths exceed the conservative 8x index reserve.
	limit := legalCorpusBytes * 9
	body, _ := json.Marshal(StorageLimit{Bytes: limit, UpdatedAt: "2026-07-16T00:00:00Z"})
	if err := atomicio.WriteDurable(StorageLimitPath(cfg.ConfigDir), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AdmitGenerationBytes(testStorageRoots(cfg), cfg.ConfigDir, "legal", storage.CeilingBytes, legalCorpusBytes, MaxShareEntries); err != nil {
		t.Fatalf("protocol-legal 50k x 4 MiB share was hard-capped: %v", err)
	}
}

func TestStorageLimitRoundTripAndCorruption(t *testing.T) {
	cfg := packetHConfig(t)
	if got, err := LoadStorageLimit(cfg.ConfigDir, 123); err != nil || got != 123 {
		t.Fatalf("absent = %d, %v", got, err)
	}
	now := time.Date(2026, 7, 16, 1, 2, 3, 0, time.FixedZone("offset", 3600))
	if err := WriteStorageLimit(cfg.ConfigDir, 456, now); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(StorageLimitPath(cfg.ConfigDir))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"bytes\": 456,\n  \"updated_at\": \"2026-07-16T00:02:03Z\"\n}\n"
	if string(body) != want {
		t.Fatalf("bytes = %q", body)
	}
	if got, err := LoadStorageLimit(cfg.ConfigDir, 123); err != nil || got != 456 {
		t.Fatalf("loaded = %d, %v", got, err)
	}
	for _, body := range []string{"{", `{"bytes":0}`} {
		if err := os.WriteFile(StorageLimitPath(cfg.ConfigDir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadStorageLimit(cfg.ConfigDir, 123); err == nil || !strings.Contains(err.Error(), StorageLimitPath(cfg.ConfigDir)) {
			t.Fatalf("corruption %q = %v", body, err)
		}
	}
}

func TestParseByteSizeContracts(t *testing.T) {
	for input, want := range map[string]int64{"15GiB": 15 << 30, " 2 mib ": 2 << 20, "3KiB": 3 << 10, "4B": 4, "5": 5, "1TiB": 1 << 40} {
		got, err := ParseByteSize(input)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q)=%d,%v want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"wat", "-1", fmt.Sprintf("%dTiB", int64(math.MaxInt64/(1<<40))+1)} {
		if _, err := ParseByteSize(input); err == nil {
			t.Errorf("ParseByteSize(%q) succeeded", input)
		}
	}
}

func TestStorageAdmissionRefusalsAndOverflow(t *testing.T) {
	cfg := packetHConfig(t)
	roots := testStorageRoots(cfg)
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "used"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewStorageAdmissionWithLimit(roots, "acme", 0).CheckCurrent(); err == nil || !strings.Contains(err.Error(), "storage-limit") {
		t.Fatalf("current refusal=%v", err)
	}
	a := NewStorageAdmissionWithLimit(roots, "acme", math.MaxInt64)
	if err := a.CheckAdditional(-1); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative=%v", err)
	}
	if err := a.CheckAdditional(math.MaxInt64); err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("overflow=%v", err)
	}
	if _, err := NewStorageAdmissionWithLimit(roots, "acme", 0).Remaining(); err == nil {
		t.Fatal("over-limit remaining succeeded")
	}
	if remaining, err := a.Remaining(); err != nil || remaining <= 0 {
		t.Fatalf("remaining=%d,%v", remaining, err)
	}
}

func TestAdmitGenerationBytesRejectsOverflow(t *testing.T) {
	cfg := packetHConfig(t)
	roots := testStorageRoots(cfg)
	for _, tc := range []struct {
		bytes   int64
		entries int
	}{{-1, 0}, {0, -1}, {math.MaxInt64, 0}, {math.MaxInt64/8 - 1, math.MaxInt32}} {
		err := AdmitGenerationBytes(roots, cfg.ConfigDir, "acme", storage.CeilingBytes, tc.bytes, tc.entries)
		if err == nil || !strings.Contains(err.Error(), "overflows") {
			t.Fatalf("AdmitGenerationBytes(%d,%d)=%v", tc.bytes, tc.entries, err)
		}
	}
}

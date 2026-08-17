package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/config"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func vaultCfg(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{VaultDir: filepath.Join(root, "vault"), DataDir: filepath.Join(root, "data")}
	if err := os.MkdirAll(cfg.VaultDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	return cfg
}
func TestVaultMarkerWriteOnce(t *testing.T) {
	cfg := vaultCfg(t)
	got, err := CreateVaultMarkerIfAbsent(cfg, "v_first", "2026-01-02T03:04:05Z", "mora test")
	if err != nil || got != "v_first" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	got, err = CreateVaultMarkerIfAbsent(cfg, "v_second", "later", "other")
	if err != nil || got != "v_first" {
		t.Fatalf("second=%q err=%v", got, err)
	}
	m, present, err := ReadVaultMarker(cfg)
	if err != nil || !present || m.Schema != 1 || m.VaultID != "v_first" || m.CreatedAt != "2026-01-02T03:04:05Z" || m.CreatedBy != "mora test" {
		t.Fatalf("marker=%+v present=%v err=%v", m, present, err)
	}
	raw, err := os.ReadFile(MarkerPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var probe VaultMarker
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("invalid json: %v: %s", err, raw)
	}
	if runtime.GOOS != "windows" {
		if mode := fileMode(t, MarkerPath(cfg)); mode != 0600 {
			t.Fatalf("mode=%#o", mode)
		}
	}
	entries, err := os.ReadDir(cfg.VaultDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json.tmp") {
			t.Fatalf("temp left: %s", e.Name())
		}
	}
}
func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
func TestVaultMarkerAbsent(t *testing.T) {
	cfg := vaultCfg(t)
	_, present, err := ReadVaultMarker(cfg)
	if err != nil || present {
		t.Fatalf("present=%v err=%v", present, err)
	}
}
func TestCorruptVaultMarkerFailsLoud(t *testing.T) {
	cfg := vaultCfg(t)
	if err := os.WriteFile(MarkerPath(cfg), []byte("{bad json"), 0600); err != nil {
		t.Fatal(err)
	}
	_, present, err := ReadVaultMarker(cfg)
	if !present || err == nil || !strings.Contains(err.Error(), "corrupt JSON") || !strings.Contains(err.Error(), "restore") || strings.Contains(err.Error(), "delete it and re-run") {
		t.Fatalf("present=%v err=%v", present, err)
	}
}
func TestReadIndexVaultIDNoIndex(t *testing.T) {
	cfg := vaultCfg(t)
	for _, c := range []config.Config{cfg, {VaultDir: cfg.VaultDir, DataDir: filepath.Join(t.TempDir(), "gone")}} {
		id, err := ReadVaultID(context.Background(), c)
		if err != nil || id != "" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	}
}
func TestReadIndexVaultIDRoundTripAndLegacy(t *testing.T) {
	cfg := vaultCfg(t)
	db, err := sql.Open("sqlite", ReadWriteDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE index_meta(key TEXT PRIMARY KEY,value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO index_meta VALUES('vault_id','v_seed')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	id, err := ReadVaultID(context.Background(), cfg)
	if err != nil || id != "v_seed" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	cfg2 := vaultCfg(t)
	db, err = sql.Open("sqlite", ReadWriteDSN(cfg2))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	id, err = ReadVaultID(context.Background(), cfg2)
	if err != nil || id != "" {
		t.Fatalf("legacy id=%q err=%v", id, err)
	}
}
func TestAssessRebuild(t *testing.T) {
	cases := []struct {
		name     string
		old, new int
		marker   string
		present  bool
		index    string
		want     RebuildDecision
	}{{"first empty", 0, 0, "", false, "", Proceed}, {"first populated", 0, 5, "", false, "", Proceed}, {"block empty", 2823, 0, "", false, "", BlockEmpty}, {"empty despite match", 2823, 0, "v_a", true, "v_a", BlockEmpty}, {"adopt no ids", 10, 9, "", false, "", Adopt}, {"adopt marker", 10, 10, "v_a", true, "", Adopt}, {"missing marker", 10, 10, "", false, "v_a", BlockIdentity}, {"foreign", 10, 9, "v_b", true, "v_a", BlockIdentity}, {"same", 10, 11, "v_a", true, "v_a", Proceed}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AssessRebuild(tc.old, tc.new, tc.marker, tc.present, tc.index); got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}
func TestBlockRecordStorageAndMessages(t *testing.T) {
	cfg := vaultCfg(t)
	if err := WriteBlockRecord(cfg, BlockEmpty, cfg.VaultDir, 3, 0, "2026-01-02T03:04:05Z"); err != nil {
		t.Fatal(err)
	}
	rec, present, err := ReadBlockRecord(cfg)
	if err != nil || !present || rec.At != "2026-01-02T03:04:05Z" || rec.Reason != "vault looked empty" || rec.OldCount != 3 || rec.NewCount != 0 {
		t.Fatalf("rec=%+v present=%v err=%v", rec, present, err)
	}
	if runtime.GOOS != "windows" {
		if mode := fileMode(t, BlockRecordPath(cfg)); mode != 0644 {
			t.Fatalf("mode=%#o", mode)
		}
	}
	if !strings.Contains(RebuildBlockMessage(BlockEmpty, cfg.VaultDir, 3), "discards the 3 indexed memories") {
		t.Fatal("empty message drift")
	}
	if err := WriteBlockRecord(cfg, BlockIdentity, cfg.VaultDir, 3, 2, "later"); err != nil {
		t.Fatal(err)
	}
	rec, present, err = ReadBlockRecord(cfg)
	if err != nil || !present || rec.Reason != "vault identity did not match the index" {
		t.Fatalf("rec=%+v present=%v err=%v", rec, present, err)
	}
	if !strings.Contains(RebuildBlockMessage(BlockIdentity, cfg.VaultDir, 3), "different vault") {
		t.Fatal("identity message drift")
	}
	if err := os.WriteFile(BlockRecordPath(cfg), []byte("bad"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, present, err = ReadBlockRecord(cfg); err != nil || present {
		t.Fatalf("corrupt advisory present=%v err=%v", present, err)
	}
	if err := ClearBlockRecord(cfg); err != nil {
		t.Fatal(err)
	}
	if err := ClearBlockRecord(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestVaultIdentityStorageErrorAndAbsentBranches(t *testing.T) {
	cfg := vaultCfg(t)
	if _, present, err := ReadBlockRecord(cfg); err != nil || present {
		t.Fatalf("absent block present=%v err=%v", present, err)
	}
	if err := os.Mkdir(BlockRecordPath(cfg), 0700); err != nil {
		t.Fatal(err)
	}
	if _, present, err := ReadBlockRecord(cfg); err == nil || present {
		t.Fatalf("directory block present=%v err=%v", present, err)
	}
	if err := os.Mkdir(MarkerPath(cfg), 0700); err != nil {
		t.Fatal(err)
	}
	if _, present, err := ReadVaultMarker(cfg); err == nil || present {
		t.Fatalf("directory marker present=%v err=%v", present, err)
	}
	bad := config.Config{VaultDir: filepath.Join(t.TempDir(), "parent-file", "vault")}
	if err := os.WriteFile(filepath.Dir(bad.VaultDir), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateVaultMarkerIfAbsent(bad, "v", "at", "by"); err == nil {
		t.Fatal("mkdir error swallowed")
	}
}
func TestReadVaultIDMissingRow(t *testing.T) {
	cfg := vaultCfg(t)
	db, err := sql.Open("sqlite", ReadWriteDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE index_meta(key TEXT PRIMARY KEY,value TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	id, err := ReadVaultID(context.Background(), cfg)
	if err != nil || id != "" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

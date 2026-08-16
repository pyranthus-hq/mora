package update

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLoadUpdateReceiptRejectsCorruptOrFutureData(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }
	validTime := now.Format(time.RFC3339)
	earlier := now.Add(-time.Minute).Format(time.RFC3339)
	future := now.Add(ClockSkew + time.Second).Format(time.RFC3339)
	tests := []struct {
		name string
		body string
	}{
		{"unknown field", `{"schema_version":1,"mystery":true}`},
		{"duplicate field", `{"schema_version":2,"schema_version":1}`},
		{"trailing value", `{"schema_version":1} {"schema_version":1}`},
		{"future schema", `{"schema_version":2}`},
		{"coercive version", `{"schema_version":1,"last_attempt_at":"` + validTime + `","last_success_at":"` + validTime + `","latest_version":"1.2","update_available":true}`},
		{"version with v", `{"schema_version":1,"last_attempt_at":"` + validTime + `","last_success_at":"` + validTime + `","latest_version":"v1.2.3","update_available":true}`},
		{"available without version", `{"schema_version":1,"update_available":true}`},
		{"latest without success", `{"schema_version":1,"latest_version":"1.2.3"}`},
		{"bad timestamp", `{"schema_version":1,"last_attempt_at":"yesterday"}`},
		{"future success", `{"schema_version":1,"last_attempt_at":"` + future + `","last_success_at":"` + future + `"}`},
		{"unknown error token", `{"schema_version":1,"last_attempt_at":"` + validTime + `","last_error_code":"token=/private/path"}`},
		{"half notification", `{"schema_version":1,"last_notified_at":"` + validTime + `"}`},
		{"notification error without availability", `{"schema_version":1,"notification_error_code":"notification_failed"}`},
		{"unknown apply outcome", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"destroyed"}`},
		{"apply private error", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"failed_before_swap","apply_error_code":"/private/path"}`},
		{"updated without rebuild decision", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","applied_at":"` + validTime + `","apply_outcome":"updated","rollback_outcome":"not_needed"}`},
		{"post-apply notification before apply", `{"schema_version":1,"last_attempt_at":"` + earlier + `","last_success_at":"` + earlier + `","latest_version":"1.2.3","last_notified_at":"` + earlier + `","last_notified_version":"1.2.3","apply_version":"1.2.3","apply_attempt_at":"` + earlier + `","applied_at":"` + validTime + `","apply_outcome":"updated","rollback_outcome":"not_needed","rebuild_outcome":"not_needed"}`},
		{"deferred claimed rollback", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"deferred","apply_error_code":"app_unwritable","rollback_outcome":"succeeded","rebuild_outcome":"not_run"}`},
		{"deferred claimed rebuild", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"deferred","apply_error_code":"app_unwritable","rollback_outcome":"not_needed","rebuild_outcome":"succeeded"}`},
		{"failed before swap claimed applied", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","applied_at":"` + validTime + `","apply_outcome":"failed_before_swap","apply_error_code":"download_failed","rollback_outcome":"not_needed","rebuild_outcome":"not_run"}`},
		{"failed before swap missing not-run rebuild", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"failed_before_swap","apply_error_code":"download_failed","rollback_outcome":"not_needed"}`},
		{"rollback repair code without failed rollback", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"rolled_back","apply_error_code":"rollback_rebuild_failed","rollback_outcome":"succeeded","rebuild_outcome":"succeeded"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := store.Path()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(); err == nil {
				t.Fatalf("corrupt receipt accepted: %s", tt.body)
			}
		})
	}
}
func TestLoadUpdateReceiptRejectsOversizedJSON(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	path := store.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := append([]byte(`{"schema_version":1}`), bytes.Repeat([]byte(" "), MaxReceiptBytes)...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("oversized receipt accepted")
	}
}
func TestSaveUpdateReceiptValidatesBeforeMutation(t *testing.T) {
	store := Store{StateDir: t.TempDir()}
	if err := store.Save(Receipt{LatestVersion: "01.2.3"}); err == nil {
		t.Fatal("invalid receipt saved")
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid save mutated receipt: %v", err)
	}
}

func TestStoreRoundTripBytesPathAndMode(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store := Store{StateDir: t.TempDir(), Now: func() time.Time { return now }}
	if got, want := store.Path(), filepath.Join(store.StateDir, "update", "status.json"); got != want {
		t.Fatalf("Path=%q want %q", got, want)
	}
	r := Receipt{LastAttemptAt: now.Format(time.RFC3339), LastSuccessAt: now.Format(time.RFC3339)}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"schema_version\": 1,\n  \"last_attempt_at\": \"2026-08-08T12:00:00Z\",\n  \"last_success_at\": \"2026-08-08T12:00:00Z\",\n  \"update_available\": false\n}\n"
	body, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("bytes=%q want %q", body, want)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 && runtime.GOOS != "windows" {
		t.Fatalf("mode=%o", got)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != SchemaVersion || loaded.LastAttemptAt != r.LastAttemptAt {
		t.Fatalf("loaded=%+v", loaded)
	}
}

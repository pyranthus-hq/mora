package mora

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func protectedSyncTestToken() string { return "0123456789abcdef" + "0123456789abcdef" }

func TestProtectedSyncReceiptRoundTrip(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	token := protectedSyncTestToken()
	want := protectedSyncReceipt{Token: token, Source: "imessage", CompletedAt: "2026-08-12T00:00:00Z"}
	if err := writeProtectedSyncReceipt(cfg, want); err != nil {
		t.Fatal(err)
	}
	got, err := readProtectedSyncReceipt(cfg, token, "imessage")
	if err != nil || got != want {
		t.Fatalf("receipt = %#v, %v", got, err)
	}
	if _, err := readProtectedSyncReceipt(cfg, token, "imessage"); err == nil {
		t.Fatal("receipt was not removed")
	}
}

func TestProtectedSyncReceiptArgs(t *testing.T) {
	cases := []struct {
		args    []string
		token   string
		want    []string
		wantErr bool
	}{
		{[]string{"imessage"}, "", []string{"imessage"}, false},
		{[]string{"imessage", protectedSyncReceiptFlag, protectedSyncTestToken()}, protectedSyncTestToken(), []string{"imessage"}, false},
		{[]string{"imessage", protectedSyncReceiptFlag}, "", nil, true},
		{[]string{"imessage", protectedSyncReceiptFlag, "bad"}, "", nil, true},
	}
	for _, tc := range cases {
		t.Run("case", func(t *testing.T) {
			got, rest, err := protectedSyncReceiptArg(tc.args)
			if (err != nil) != tc.wantErr || got != tc.token || !sameStrings(rest, tc.want) {
				t.Fatalf("got %q %v %v", got, rest, err)
			}
		})
	}
}
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRelayProtectedSync(t *testing.T) {
	oldExe, oldOpen, oldGOOS, oldNow := protectedSyncExecutable, protectedSyncRunOpen, runtimeGOOS, protectedSyncNow
	t.Cleanup(func() {
		protectedSyncExecutable, protectedSyncRunOpen, runtimeGOOS, protectedSyncNow = oldExe, oldOpen, oldGOOS, oldNow
	})
	runtimeGOOS = func() string { return "darwin" }
	app := filepath.Join(t.TempDir(), "Mora.app")
	exe := filepath.Join(app, "Contents", "MacOS", "mora")
	protectedSyncExecutable = func() (string, error) { return exe, nil }
	protectedSyncNow = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	cfg := Config{StateDir: t.TempDir()}
	protectedSyncRunOpen = func(_ context.Context, args ...string) error {
		token := args[len(args)-1]
		return writeProtectedSyncReceipt(cfg, protectedSyncReceipt{Token: token, Source: "imessage", CompletedAt: protectedSyncNow().UTC().Format(time.RFC3339)})
	}
	if err := relayProtectedSync(context.Background(), cfg, "imessage"); err != nil {
		t.Fatal(err)
	}
	protectedSyncRunOpen = func(context.Context, ...string) error { return errors.New("open failed") }
	if err := relayProtectedSync(context.Background(), cfg, "imessage"); err == nil {
		t.Fatal("open failure accepted")
	}
}

func TestProtectedSyncReceiptRejectsUnprotectedSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	err := Run(context.Background(), []string{"sync", "google", protectedSyncReceiptFlag, protectedSyncTestToken()}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "only valid") {
		t.Fatalf("unprotected receipt error = %v", err)
	}
}

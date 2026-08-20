package protectedsync

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testToken() string { return strings.Repeat("ab", 16) }
func TestProtectedSyncReceiptRoundTrip(t *testing.T) {
	state := t.TempDir()
	want := Receipt{Token: testToken(), Source: "imessage", CompletedAt: "2026-08-12T00:00:00Z"}
	if err := WriteReceipt(state, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path(state, want.Token))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	got, err := ReadReceipt(state, want.Token, "imessage")
	if err != nil || got != want {
		t.Fatalf("receipt=%#v %v", got, err)
	}
	if _, err := ReadReceipt(state, want.Token, "imessage"); err == nil {
		t.Fatal("receipt was not removed")
	}
}
func TestProtectedSyncReceiptArgs(t *testing.T) {
	cases := []struct {
		args  []string
		token string
		want  []string
		bad   bool
	}{{[]string{"imessage"}, "", []string{"imessage"}, false}, {[]string{"imessage", ReceiptFlag, testToken()}, testToken(), []string{"imessage"}, false}, {[]string{"imessage", ReceiptFlag}, "", nil, true}, {[]string{"imessage", ReceiptFlag, "bad"}, "", nil, true}, {[]string{ReceiptFlag, testToken(), ReceiptFlag, testToken()}, "", nil, true}, {[]string{ReceiptFlag, strings.ToUpper(testToken())}, "", nil, true}}
	for _, tc := range cases {
		got, rest, err := ParseArgs(tc.args)
		if (err != nil) != tc.bad || got != tc.token || strings.Join(rest, "|") != strings.Join(tc.want, "|") {
			t.Fatalf("args=%v got=%q %v err=%v", tc.args, got, rest, err)
		}
	}
}
func TestRelayProtectedSync(t *testing.T) {
	state := t.TempDir()
	app := filepath.Join(t.TempDir(), "Mora.app")
	exe := filepath.Join(app, "Contents", "MacOS", "mora")
	o := Options{StateDir: state, Source: "imessage", GOOS: "darwin", Executable: func() (string, error) { return exe, nil }, EvalSymlinks: func(s string) (string, error) { return s, nil }, AppRoot: func(string) (string, bool) { return app, true }, Rand: bytes.NewReader(bytes.Repeat([]byte{1}, 16))}
	o.RunOpen = func(_ context.Context, args ...string) error {
		token := args[len(args)-1]
		return WriteReceipt(state, Receipt{Token: token, Source: "imessage", CompletedAt: "2026-01-01T00:00:00Z"})
	}
	if err := Relay(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o.RunOpen = func(context.Context, ...string) error { return errors.New("open failed") }
	o.Rand = bytes.NewReader(bytes.Repeat([]byte{1}, 16))
	if err := Relay(context.Background(), o); err == nil || !strings.Contains(err.Error(), "launching Mora.app") {
		t.Fatalf("open err=%v", err)
	}
	o.GOOS = "linux"
	if err := Relay(context.Background(), o); !errors.Is(err, ErrDirect) {
		t.Fatalf("direct err=%v", err)
	}
}
func TestReceiptValidationFailures(t *testing.T) {
	state := t.TempDir()
	if err := WriteReceipt(state, Receipt{Token: "bad", Source: "imessage"}); err == nil {
		t.Fatal("bad token accepted")
	}
	token := testToken()
	if err := WriteReceipt(state, Receipt{Token: token, Source: "imessage"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReceipt(state, token, "applecalendar"); err == nil {
		t.Fatal("mismatched source accepted")
	}
	o := Options{StateDir: state, Source: "imessage", GOOS: "darwin", Executable: func() (string, error) { return "", errors.New("missing") }, AppRoot: func(string) (string, bool) { return "", false }}
	if err := Relay(context.Background(), o); !errors.Is(err, ErrDirect) {
		t.Fatalf("executable fallback=%v", err)
	}
}

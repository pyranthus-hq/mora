package mora

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func reviewAppKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func reviewAppPEM(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// reviewAppSigned returns app-keep arguments signed by k at time at.
func reviewAppSigned(t *testing.T, k *ecdsa.PrivateKey, kind, id, digest string, at time.Time) []string {
	t.Helper()
	cfg := mustConfig(t)
	h := sha256.Sum256(statusApprovalMessage(cfg.ConfigDir, cfg.VaultDir, kind, id, digest, at.Unix()))
	sig, err := ecdsa.SignASN1(rand.Reader, k, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return []string{kind, id, digest, strconv.FormatInt(at.Unix(), 10), base64.StdEncoding.EncodeToString(sig)}
}

func reviewAppCaller(t *testing.T, err error) {
	t.Helper()
	prev := verifyStatusAppCaller
	verifyStatusAppCaller = func() error { return err }
	t.Cleanup(func() { verifyStatusAppCaller = prev })
}

func reviewAppRows(t *testing.T) map[string]reviewRow {
	t.Helper()
	var got struct {
		Notes []reviewRow `json:"notes"`
	}
	if err := json.Unmarshal([]byte(run(t, "review", "--list", "--json")), &got); err != nil {
		t.Fatal(err)
	}
	rows := map[string]reviewRow{}
	for _, r := range got.Notes {
		rows[r.ID] = r
	}
	return rows
}

func reviewAppKeep(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(testCtx(t), append([]string{"review", "app-keep"}, args...), &out, &out, strings.NewReader(""))
	return out.String(), err
}

func TestReviewAppKeepOwnerConfirmsOnlyWithASignedApproval(t *testing.T) {
	cfg := reviewSetup(t)
	clearAgentEnv(t)
	saved := reviewSaved(t, "Saved note")
	held := reviewHeld(t, "Held note")
	rows := reviewAppRows(t)
	if rows[saved].Confirm == "" || len(rows[saved].Fields) == 0 || rows[held].Confirm == "" {
		t.Fatalf("list must carry the digest and its fields: %+v", rows)
	}
	for _, f := range rows[saved].Fields {
		if f[0] == "text" {
			t.Fatal("list must not carry the text field")
		}
	}
	key, other := reviewAppKey(t), reviewAppKey(t)
	now := time.Now()
	good := reviewAppSigned(t, key, "saved", saved, rows[saved].Confirm, now)
	reviewAppCaller(t, nil)

	if _, err := reviewAppKeep(t, good...); err == nil || !strings.Contains(err.Error(), "no trusted Mora Status key") {
		t.Fatalf("untrusted err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, statusAppKeyFile), reviewAppPEM(t, key), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := map[string][]string{
		"does not match": reviewAppSigned(t, other, "saved", saved, rows[saved].Confirm, now),
		"not fresh":      reviewAppSigned(t, key, "saved", saved, rows[saved].Confirm, now.Add(-10*time.Minute)),
		"changed after":  reviewAppSigned(t, key, "saved", saved, strings.Repeat("0", 64), now),
	}
	// An approval for this Mora does not count in another vault (MORA_VAULT changed by a wrapper).
	hv := sha256.Sum256(statusApprovalMessage(cfg.ConfigDir, "/elsewhere/vault", "saved", saved, rows[saved].Confirm, now.Unix()))
	if vsig, err := ecdsa.SignASN1(rand.Reader, key, hv[:]); err == nil {
		refused["signature does not match the trusted"] = []string{"saved", saved, rows[saved].Confirm, strconv.FormatInt(now.Unix(), 10), base64.StdEncoding.EncodeToString(vsig)}
	}
	// A signature for one note does not keep another, or the same note as another kind.
	swapped := append([]string(nil), good...)
	swapped[0] = "held"
	refused["signature does not match"] = swapped
	for want, args := range refused {
		if _, err := reviewAppKeep(t, args...); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v", want, err)
		}
	}
	reviewAppCaller(t, errors.New("the caller was not started by Mora Status"))
	if _, err := reviewAppKeep(t, good...); err == nil || !strings.Contains(err.Error(), "Only the signed Mora Status app") {
		t.Fatalf("non-app caller err = %v", err)
	}
	reviewAppCaller(t, nil)
	t.Setenv("CLAUDECODE", "1")
	if _, err := reviewAppKeep(t, good...); err == nil || !strings.Contains(err.Error(), "agent session") {
		t.Fatalf("agent session err = %v", err)
	}
	clearAgentEnv(t)
	if m, _ := findMemory(cfg, saved); memory.IsOwnerConfirmed(m) || !memory.IsTentative(m) {
		t.Fatal("a refused call changed the note")
	}

	if out, err := reviewAppKeep(t, good...); err != nil || !strings.Contains(out, "owner-confirmed") {
		t.Fatalf("saved keep: %q %v", out, err)
	}
	if m, _ := findMemory(cfg, saved); !memory.IsOwnerConfirmed(m) || memory.IsTentative(m) {
		t.Fatal("saved note is not owner-confirmed")
	}
	out, err := reviewAppKeep(t, reviewAppSigned(t, key, "held", held, rows[held].Confirm, now)...)
	if err != nil || !strings.Contains(out, "owner-confirmed memory") {
		t.Fatalf("held keep: %q %v", out, err)
	}
	if len(reviewAppRows(t)) != 0 {
		t.Fatal("kept notes still wait for review")
	}
}

func TestReviewListMasksCredentialFieldsAndDropsTheDigest(t *testing.T) {
	reviewSetup(t)
	// Built at run time so the repository's secret scanner sees no token.
	fake := "gh" + "p_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	held := reviewHeld(t, "token "+fake)
	row := reviewAppRows(t)[held]
	if row.Confirm != "" || strings.Contains(row.Title, fake) {
		t.Fatalf("a masked note must not carry a digest or the raw title: %+v", row)
	}
	for _, f := range row.Fields {
		if strings.Contains(f[1], fake) {
			t.Fatalf("field %q leaked the credential", f[0])
		}
	}
}

func TestReviewTrustAppNeedsTheOwnerTerminalAndAYes(t *testing.T) {
	cfg := reviewSetup(t)
	key := reviewAppKey(t)
	path := defaultStatusAppPublicKey()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, reviewAppPEM(t, key), 0o600); err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(cfg.ConfigDir, statusAppKeyFile)
	var out bytes.Buffer
	t.Setenv("CLAUDECODE", "1")
	if err := Run(testCtx(t), []string{"review", "trust-app"}, &out, &out, strings.NewReader("y\n")); err == nil {
		t.Fatal("trust-app ran inside an agent session")
	}
	reviewTerminal(t)
	if err := Run(testCtx(t), []string{"review", "trust-app"}, &out, &out, strings.NewReader("n\n")); err == nil {
		t.Fatal("trust-app saved without a yes")
	}
	if _, err := os.Stat(trusted); err == nil {
		t.Fatal("a refused trust-app wrote the key")
	}
	out.Reset()
	if err := Run(testCtx(t), []string{"review", "trust-app"}, &out, &out, strings.NewReader("y\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), statusAppKeyFingerprint(&key.PublicKey)) {
		t.Fatalf("fingerprint not shown: %q", out.String())
	}
	if got, err := readStatusAppKey(trusted); err != nil || !got.Equal(&key.PublicKey) {
		t.Fatalf("trusted key = %v, %v", got, err)
	}
}

func TestReviewAppKeepUsage(t *testing.T) {
	reviewSetup(t)
	reviewAppCaller(t, nil)
	for _, args := range [][]string{{}, {"saved", "x", "d"}, {"other", "x", "d", "1", "AA=="}, {"saved", "x", "", "1", "AA=="}, {"saved", "x", "d", "soon", "AA=="}, {"saved", "x", "d", "1", "!!"}} {
		if _, err := reviewAppKeep(t, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

// testdata/status_approval.json comes from apps/mora-menubar/Tests/main.swift
// (CryptoKit): it proves the Swift message, signature and fingerprint match.
func TestStatusApprovalMatchesTheSwiftApp(t *testing.T) {
	b, err := os.ReadFile("testdata/status_approval.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ PEM, Signature, Fingerprint string }
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	pub, err := parseStatusAppKey([]byte(f.PEM))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.StdEncoding.DecodeString(f.Signature)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(statusApprovalMessage("/c", "/v", "held", "p_1", "cd", 18))
	if !ecdsa.VerifyASN1(pub, h[:], sig) {
		t.Fatal("the Swift approval signature does not verify")
	}
	if got := statusAppKeyFingerprint(pub); got != f.Fingerprint {
		t.Fatalf("fingerprint %q, Swift shows %q", got, f.Fingerprint)
	}
}

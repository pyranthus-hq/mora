package mora

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

const (
	reviewAppUsage      = "usage: mora review app-keep <saved|held> <id> <owner-digest> <unix-time> <signature> (Mora Status only)"
	reviewTrustUsage    = "usage: mora review trust-app"
	statusAppKeyFile    = "mora-status-approval.pem"
	statusApprovalLabel = "mora-status-keep-mine/v1"
	statusApprovalSkew  = 5 * time.Minute
)

// verifyStatusAppCaller reports whether this process was started by the
// collector inside the signed Mora Status app, while that app is running. A
// variable so tests can stand in for the platform check.
var verifyStatusAppCaller = statusAppCaller

var reviewAppNow = time.Now

// statusApprovalMessage is the exact byte string the app's Secure Enclave key
// signs after Touch ID or the login password, one note at a time.
// It names this Mora's config directory and vault, so an approval cannot be
// replayed into another Mora by changing MORA_CONFIG_DIR or MORA_VAULT.
func statusApprovalMessage(configDir, vault, kind, id, digest string, unix int64) []byte {
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s\n%d\n", statusApprovalLabel, configDir, vault, kind, id, digest, unix))
}

// cmdReviewAppKeep is the one entry point that lets Mora Status make an agent
// note owner-confirmed. The app shows every field the digest binds, then signs
// (config dir, vault, kind, id, digest, time) with a Secure Enclave key whose every use needs
// Touch ID or the login password. Mora accepts the keep only with a valid
// signature from the key the owner trusted in a terminal (`mora review
// trust-app`), made in the last five minutes. Process ancestry is checked too
// (the parent must be the app's signed collector and its parent the signed
// app), but it is not the proof: the collector runs mora from MORA_BIN, so an
// agent could start the app with its own wrapper. The signature is the proof.
// The digest must equal the note's current digest, and the keep writes from
// the same read, so a note that changed after the app showed it is refused.
func cmdReviewAppKeep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 5 || (args[0] != reviewKindSaved && args[0] != reviewKindHeld) || args[2] == "" {
		return errors.New(reviewAppUsage)
	}
	kind, id, digest := args[0], args[1], args[2]
	unix, err := strconv.ParseInt(args[3], 10, 64)
	if err != nil {
		return errors.New(reviewAppUsage)
	}
	sig, err := base64.StdEncoding.DecodeString(args[4])
	if err != nil {
		return errors.New(reviewAppUsage)
	}
	if err := refuseInAgentSession("mora review app-keep"); err != nil {
		return err
	}
	if err := refuseDashLedPositional("review app-keep", "note id", id); err != nil {
		return err
	}
	if err := verifyStatusAppCaller(); err != nil {
		return fmt.Errorf("refusing mora review app-keep: %v. Only the signed Mora Status app can use it; in a terminal, run mora review", err)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	pub, err := readStatusAppKey(filepath.Join(cfg.ConfigDir, statusAppKeyFile))
	if err != nil {
		return fmt.Errorf("refusing mora review app-keep: no trusted Mora Status key (%v). Run mora review trust-app in your own terminal once", err)
	}
	if d := reviewAppNow().Sub(time.Unix(unix, 0)); d > statusApprovalSkew || d < -statusApprovalSkew {
		return errors.New("refusing mora review app-keep: the approval is not fresh; approve again in Mora Status")
	}
	if strings.ContainsAny(cfg.ConfigDir+cfg.VaultDir, "\n\r") {
		return errors.New("refusing mora review app-keep: the config or vault path holds a line break")
	}
	h := sha256.Sum256(statusApprovalMessage(cfg.ConfigDir, cfg.VaultDir, kind, id, digest, unix))
	if !ecdsa.VerifyASN1(pub, h[:], sig) {
		return errors.New("refusing mora review app-keep: the approval signature does not match the trusted Mora Status key")
	}
	n, err := loadReviewNote(cfg, kind, id)
	if err != nil {
		return err
	}
	if n.Confirm != digest {
		return fmt.Errorf("note %s changed after it was shown; nothing was done", rulesShow(id))
	}
	if kind == reviewKindHeld {
		defer drainReconcileOrWarn(stderr)
	}
	return reviewAct(ctx, cfg, n, true, stdout, stderr)
}

func parseStatusAppKey(b []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("not a PEM public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("not a P-256 key")
	}
	return pub, nil
}

func readStatusAppKey(path string) (*ecdsa.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseStatusAppKey(b)
}

func defaultStatusAppPublicKey() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Library/Application Support/Mora Status/approval-key.pem")
}

// cmdReviewTrustApp records the Mora Status approval key for this Mora, after
// the owner reads its fingerprint and types yes in their own terminal. The
// fingerprint is also shown in the app. Its limit is the terminal gate's limit
// (requireMineTerminal): a program in a fake terminal passes, exactly as it can
// already run `mora confirm --mine`. Mora cannot tell a Secure Enclave key
// from a software key, so this path is no stronger than that one.
func cmdReviewTrustApp(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) != 0 {
		return errors.New(reviewTrustUsage)
	}
	if err := requireMineTerminal(stdin, "mora review trust-app"); err != nil {
		return err
	}
	path := defaultStatusAppPublicKey()
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read the Mora Status key: %w. Open Mora Status and press Keep as my own words once to make it", err)
	}
	pub, err := parseStatusAppKey(b)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Mora Status key fingerprint: %s\n", statusAppKeyFingerprint(pub))
	fmt.Fprintf(stderr, "Notes kept in Mora Status with Keep as my own words will count as your own statements in %s.\n", cfg.ConfigDir)
	fmt.Fprint(stderr, "Trust this key? [y/N] ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	fmt.Fprintln(stderr)
	if a := strings.ToLower(strings.TrimSpace(line)); err != nil || (a != "y" && a != "yes") {
		return errors.New("Not trusted. Nothing was changed.")
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if err := atomicio.Write(filepath.Join(cfg.ConfigDir, statusAppKeyFile), out, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "trusted Mora Status key %s\n", statusAppKeyFingerprint(pub))
	return nil
}

// statusAppKeyFingerprint is the first 16 bytes of the SHA-256 of the key's
// PKIX encoding, in hex groups; the app shows the same value.
func statusAppKeyFingerprint(pub *ecdsa.PublicKey) string {
	der, _ := x509.MarshalPKIXPublicKey(pub)
	sum := sha256.Sum256(der)
	h := hex.EncodeToString(sum[:16])
	parts := []string{}
	for i := 0; i < len(h); i += 4 {
		parts = append(parts, h[i:i+4])
	}
	return strings.Join(parts, " ")
}

package mora

// CLI glue for the bucket share transport: flag parsing and the push/subscribe/
// pull flows that share.go dispatches to when a share's transport is a bucket.
// git stays the default and unchanged; `--via r2|s3|bucket` opts into a bucket.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
)

// bucketOf returns the bucket config if this ref selects a bucket transport, else
// nil (git). The single place the git/bucket decision is made.
func bucketOf(t *transportRef) *bucketConfig {
	if t != nil && t.Kind == "bucket" && t.Bucket != nil {
		return t.Bucket
	}
	return nil
}

// display is a human-readable, credential-free rendering of the destination.
func (c bucketConfig) display() string {
	loc := c.Bucket
	if p := strings.Trim(c.Prefix, "/"); p != "" {
		loc += "/" + p
	}
	if c.Endpoint != "" {
		loc = strings.TrimRight(c.Endpoint, "/") + "/" + loc
	}
	return loc
}

// transportFlags registers the shared --via/bucket flags on a FlagSet.
type transportFlags struct {
	via, bucket, endpoint, region, prefix, secretRef *string
}

func registerTransportFlags(fs *flag.FlagSet) transportFlags {
	return transportFlags{
		via:       fs.String("via", "", "transport: git (default) | r2 | s3 | bucket"),
		bucket:    fs.String("bucket", "", "S3/R2 bucket name (with --via r2|s3|bucket)"),
		endpoint:  fs.String("endpoint", "", "S3-compatible endpoint URL (e.g. Cloudflare R2 / Backblaze B2)"),
		region:    fs.String("region", "", `bucket region (default "auto")`),
		prefix:    fs.String("prefix", "", "object key prefix within the bucket"),
		secretRef: fs.String("secret-ref", "", "env-var prefix for credentials (default MORA_SHARE)"),
	}
}

func (tf transportFlags) isBucket() bool {
	switch *tf.via {
	case "r2", "s3", "b2", "bucket":
		return true
	default:
		return false
	}
}

// resolve returns a bucket transportRef, or nil for the git default.
func (tf transportFlags) resolve() (*transportRef, error) {
	if *tf.via != "" && !tf.isBucket() && *tf.via != "git" {
		return nil, fmt.Errorf("unknown --via %q (want git | r2 | s3 | b2 | bucket)", *tf.via)
	}
	if !tf.isBucket() {
		return nil, nil
	}
	if *tf.bucket == "" {
		return nil, fmt.Errorf("--via %s requires --bucket <name>", *tf.via)
	}
	return &transportRef{Kind: "bucket", Bucket: &bucketConfig{
		Endpoint: *tf.endpoint, Region: *tf.region, Bucket: *tf.bucket,
		Prefix: *tf.prefix, SecretRef: *tf.secretRef,
	}}, nil
}

// shareInitBucket records a bucket publish grant. Unlike git init there is no
// staging repo to create — the signed manifest is written to the bucket on the
// first `share push`.
func shareInitBucket(cfg Config, name, scope string, recipients []string, owner string, tref *transportRef, stdout io.Writer) error {
	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	for _, p := range sf.Publishes {
		if p.Name == name {
			return fmt.Errorf("share %q already exists — remove it first with `mora share remove %s --yes`", name, name)
		}
	}
	for _, s := range sf.Subscriptions {
		if s.Name == name {
			return fmt.Errorf("%q already names a subscription — share and subscription names share one namespace", name)
		}
	}
	sf.Publishes = append(sf.Publishes, sharePublish{
		Name: name, Scope: scope, Recipients: recipients, Transport: tref, Owner: owner,
		CreatedAt: time.Now().Format(time.RFC3339),
	})
	if err := saveShares(cfg, sf); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "share %q initialized — scope %s, %d recipient key(s), bucket %s. Publish with `mora share push %s`.\n",
		name, scope, len(recipients), redactCredentials(bucketOf(tref).display()), name)
	fmt.Fprintln(stdout, shareInitDisclosure)
	fmt.Fprintln(stdout, "    Publish from ONE machine at a time — concurrent pushes to the same bucket can")
	fmt.Fprintln(stdout, "    corrupt the share (single-writer).")
	return nil
}

// sharePushBucket previews the full set (P0: nothing leaves before the preview +
// confirm), then publishes to the bucket.
func sharePushBucket(ctx context.Context, cfg Config, pub sharePublish, mems []Memory, recips []age.Recipient, bc bucketConfig, stdout io.Writer, stdin io.Reader, yes bool) error {
	fmt.Fprintf(stdout, "share %q — scope %s: %d memories → %s (age-encrypted to %d recipient key(s))\n",
		pub.Name, pub.Scope, len(mems), redactCredentials(bc.display()), len(recips))
	for _, m := range mems {
		fmt.Fprintf(stdout, "  • %s\t%s\n", m.ID, m.Title)
	}
	if len(mems) == 0 {
		fmt.Fprintln(stdout, "  (nothing in this scope to publish)")
	}
	fmt.Fprintf(stdout, "full content: `mora share preview %s`\n", pub.Name)
	if !yes {
		if err := confirmSharePushFn(stdin, stdout, pub.Name); err != nil {
			return err
		}
	}
	priv, err := shareSigningKey(cfg)
	if err != nil {
		return err
	}
	store, err := newObjectStore(bc)
	if err != nil {
		return err
	}
	if err := bucketPublish(ctx, store, bc, pub, mems, priv, recips); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "share %q published to bucket %s.\n", pub.Name, bc.Bucket)
	return nil
}

// shareSubscribeBucket clones no repo: it fetches + verifies the signed manifest,
// requires the out-of-band --confirm-pin to match the publisher's fingerprint
// (a pasted bucket URL is a MITM-able first-contact channel), then imports.
func shareSubscribeBucket(ctx context.Context, cfg Config, name string, bc bucketConfig, confirmPin string, stdout io.Writer) error {
	confirmPin = strings.TrimSpace(confirmPin)
	ids, err := loadShareIdentities(cfg)
	if err != nil {
		return err
	}
	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	for _, s := range sf.Subscriptions {
		if s.Name == name {
			return fmt.Errorf("subscription %q already exists — `mora share pull %s` updates it", name, name)
		}
	}
	for _, p := range sf.Publishes {
		if p.Name == name {
			return fmt.Errorf("%q already names a share you publish — share and subscription names share one namespace", name)
		}
	}
	store, err := newObjectStore(bc)
	if err != nil {
		return err
	}
	sub := shareSubscription{Name: name, Transport: &transportRef{Kind: "bucket", Bucket: &bc}, CreatedAt: time.Now().Format(time.RFC3339)}
	// First-contact fingerprint confirmation runs against a throwaway probe fetch
	// BEFORE any generation is built, so an unconfirmed pin never publishes.
	probe := filepath.Join(shareSubRoot(cfg, name), "fetch-probe")
	_ = os.RemoveAll(probe)
	if err := os.MkdirAll(probe, 0o700); err != nil {
		return err
	}
	pin, _, err := bucketFetch(ctx, store, bc, sub, ids, probe)
	_ = os.RemoveAll(probe)
	if err != nil {
		_ = os.RemoveAll(shareSubRoot(cfg, name))
		return fmt.Errorf("%w — nothing was registered; fix the cause (has the publisher pushed? is your key among the recipients?) and re-run `mora share subscribe`", err)
	}
	fp := signPubFingerprint(pin)
	if confirmPin == "" {
		_ = os.RemoveAll(shareSubRoot(cfg, name))
		return fmt.Errorf("first contact: confirm the publisher fingerprint out of band, then re-run with --confirm-pin %s", fp)
	}
	if confirmPin != fp {
		_ = os.RemoveAll(shareSubRoot(cfg, name))
		return fmt.Errorf("--confirm-pin %s does not match this share's publisher fingerprint %s — refusing (possible impostor)", confirmPin, fp)
	}
	var stats shareImportStats
	var gotPin ed25519.PublicKey
	var gotVer int
	err = shareBuildAndPublish(ctx, cfg, name, buildModeImport, func(runID string) (int, error) {
		seq, st, p, v, ierr := bucketShareImport(ctx, cfg, sub, bc, runID)
		stats, gotPin, gotVer = st, p, v
		return seq, ierr
	})
	if err != nil {
		_ = os.RemoveAll(shareSubRoot(cfg, name))
		return fmt.Errorf("%w — nothing was registered", err)
	}
	sub.PinnedPubkey, sub.LastVersion = gotPin, gotVer
	sf.Subscriptions = append(sf.Subscriptions, sub)
	if err := saveShares(cfg, sf); err != nil {
		return err
	}
	owner := stats.Owner
	if owner == "" {
		owner = "(unnamed publisher)"
	}
	fmt.Fprintf(stdout, "subscribed to bucket share %q — %d memories from %s (scope %s), read-only beside your vault.\n", name, stats.Total, owner, stats.Scope)
	fmt.Fprintf(stdout, "shared results appear in search/think attributed as [%s]; your own vault and graph are never modified.\n", name)
	return nil
}

// sharePullBucket re-fetches one bucket subscription, advancing its pinned version.
func sharePullBucket(ctx context.Context, cfg Config, sub shareSubscription, bc bucketConfig, stdout io.Writer) error {
	var stats shareImportStats
	var pin ed25519.PublicKey
	var ver int
	if err := shareBuildAndPublish(ctx, cfg, sub.Name, buildModeImport, func(runID string) (int, error) {
		seq, st, p, v, ierr := bucketShareImport(ctx, cfg, sub, bc, runID)
		stats, pin, ver = st, p, v
		return seq, ierr
	}); err != nil {
		return err
	}
	// Reconcile LastVersion up to the durable committed floor (belt-and-suspenders
	// after a crash could lose this update — the committed BucketFloor already
	// fences replays).
	if err := updateSubscriptionState(cfg, sub.Name, pin, ver); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "share %q: %d new/updated, %d removed, %d total.\n", sub.Name, stats.Imported, stats.Removed, stats.Total)
	return nil
}

// updateSubscriptionState persists the advanced TOFU pin + version after a pull.
func updateSubscriptionState(cfg Config, name string, pin ed25519.PublicKey, ver int) error {
	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	for i := range sf.Subscriptions {
		if sf.Subscriptions[i].Name == name {
			sf.Subscriptions[i].PinnedPubkey = pin
			sf.Subscriptions[i].LastVersion = ver
			return saveShares(cfg, sf)
		}
	}
	return errors.New("subscription vanished during pull")
}

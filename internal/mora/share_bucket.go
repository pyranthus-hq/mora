package mora

import (
	"context"
	"crypto/ed25519"
	"filippo.io/age"
	"github.com/pyranthus-hq/mora/internal/memory"
	sharingpkg "github.com/pyranthus-hq/mora/internal/sharing"
)

const shareManifestObject = sharingpkg.ManifestObject
const shareMaxShareEntries = sharingpkg.MaxShareEntries
const shareManifestSchema = sharingpkg.ExportManifestSchema
const shareMaxMemoryBytes = sharingpkg.MaxMemoryBytes

type shareManifest = sharingpkg.ExportManifest

var shareExportIDRE = sharingpkg.ExportIDRE
var errObjectNotFound = sharingpkg.ErrObjectNotFound

type objectStore interface {
	putObject(context.Context, string, []byte) error
	getObject(context.Context, string) ([]byte, error)
	listKeys(context.Context, string) ([]string, error)
	deleteObject(context.Context, string) error
}
type sharingObjectStore struct{ objectStore }

func (s sharingObjectStore) PutObject(ctx context.Context, key string, data []byte) error {
	return s.putObject(ctx, key, data)
}
func (s sharingObjectStore) GetObject(ctx context.Context, key string) ([]byte, error) {
	return s.getObject(ctx, key)
}
func (s sharingObjectStore) ListKeys(ctx context.Context, prefix string) ([]string, error) {
	return s.listKeys(ctx, prefix)
}
func (s sharingObjectStore) DeleteObject(ctx context.Context, key string) error {
	return s.deleteObject(ctx, key)
}

func bucketPublish(ctx context.Context, store objectStore, cfg bucketConfig, pub sharePublish, mems []memory.Memory, priv ed25519.PrivateKey, recips []age.Recipient) error {
	return sharingpkg.PublishBucket(ctx, sharingObjectStore{store}, cfg, pub, mems, priv, recips, "mora "+BuildVersion)
}
func bucketFetch(ctx context.Context, store objectStore, cfg bucketConfig, sub shareSubscription, ids []age.Identity, dest string) (ed25519.PublicKey, int, error) {
	return sharingpkg.FetchBucket(ctx, sharingObjectStore{store}, cfg, sub, ids, dest)
}

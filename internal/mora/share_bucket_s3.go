package mora

import (
	"context"
	sharingpkg "github.com/pyranthus-hq/mora/internal/sharing"
)

type legacyObjectStore struct{ sharingpkg.ObjectStore }

func (s legacyObjectStore) putObject(ctx context.Context, key string, data []byte) error {
	return s.PutObject(ctx, key, data)
}
func (s legacyObjectStore) getObject(ctx context.Context, key string) ([]byte, error) {
	return s.GetObject(ctx, key)
}
func (s legacyObjectStore) listKeys(ctx context.Context, prefix string) ([]string, error) {
	return s.ListKeys(ctx, prefix)
}
func (s legacyObjectStore) deleteObject(ctx context.Context, key string) error {
	return s.DeleteObject(ctx, key)
}
func newObjectStore(cfg bucketConfig) (objectStore, error) {
	store, err := sharingpkg.NewObjectStore(cfg)
	if err != nil {
		return nil, err
	}
	return legacyObjectStore{store}, nil
}

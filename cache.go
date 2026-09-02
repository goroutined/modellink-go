package modellink

import (
	"context"
	"errors"
)

// CacheEntry is one immutable, versioned ModelLink data package. Files contain
// the verified JSON bytes published in @modellink/data.
type CacheEntry struct {
	Version string
	Files   map[DataFile][]byte
}

// CacheStore persists versioned packages and the currently active version.
// Put must publish an entry atomically: readers must never observe partial data.
type CacheStore interface {
	Current(ctx context.Context) (string, error)
	Get(ctx context.Context, version string) (*CacheEntry, error)
	Put(ctx context.Context, entry *CacheEntry) error
	SetCurrent(ctx context.Context, version string) error
}

// Locker coordinates a complete update operation across all clients sharing a
// cache. Implementations must honor cancellation while waiting for a lock.
type Locker interface {
	Lock(ctx context.Context, key string) (Lock, error)
}

// Lock is an acquired cache lock.
type Lock interface {
	Unlock() error
}

// Cache combines storage and update coordination. Most users only configure
// this interface; the smaller interfaces allow advanced backend composition.
type Cache interface {
	CacheStore
	Locker
}

// CachePruner is an optional cache capability used after successful package
// operations. Custom caches may omit it and manage retention independently.
type CachePruner interface {
	Prune(ctx context.Context, protected ...string) error
}

type combinedCache struct {
	CacheStore
	Locker
}

func (cache *combinedCache) Prune(ctx context.Context, protected ...string) error {
	if pruner, ok := cache.CacheStore.(CachePruner); ok {
		return pruner.Prune(ctx, protected...)
	}
	return nil
}

// NewCache combines a store and locker into one Cache.
func NewCache(store CacheStore, locker Locker) (Cache, error) {
	if store == nil {
		return nil, errors.New("modellink: cache store is nil")
	}
	if locker == nil {
		return nil, errors.New("modellink: cache locker is nil")
	}
	return &combinedCache{CacheStore: store, Locker: locker}, nil
}

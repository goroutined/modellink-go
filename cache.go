package modellink

import (
	"context"
	"errors"
	"time"
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
	// SetCurrent must atomically update Version and LastUpdate, preserving
	// LastCheck. Same-version activation must preserve LastUpdate. Implementations
	// coordinate metadata writes internally (callers can hold version locks).
	SetCurrent(ctx context.Context, version string) error
	// State returns durable metadata; a new cache returns an empty state.
	State(ctx context.Context) (CacheState, error)
	// RecordCheck merges a successful latest lookup without overwriting activation.
	// Older CheckedAt values must not replace newer records.
	RecordCheck(ctx context.Context, check CheckState) error
}

// CheckState records a successful latest lookup, not a successful download.
type CheckState struct {
	CheckedAt     time.Time `json:"checked_at"`
	LatestVersion string    `json:"latest_version"`
	Registry      string    `json:"registry"`
}

// UpdateState records the last actual change of the shared active pointer.
type UpdateState struct {
	UpdatedAt       time.Time `json:"updated_at"`
	PreviousVersion string    `json:"previous_version"`
	CurrentVersion  string    `json:"current_version"`
}

// CacheState survives process restarts. Nil records mean no known event.
// Version uses the legacy current.json field for backwards-readable storage.
type CacheState struct {
	Version    string       `json:"version,omitempty"`
	LastCheck  *CheckState  `json:"last_check,omitempty"`
	LastUpdate *UpdateState `json:"last_update,omitempty"`
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

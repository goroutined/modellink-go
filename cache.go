package modellink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gofrs/flock"
	"github.com/goroutined/modellink-go/internal/artifact"
)

var cachedFiles = []string{
	string(FileManifest),
	string(FileAPI),
	string(FileModels),
	string(FileCatalog),
	string(FileSchema),
}

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

// FileCache is the default persistent cache. It uses atomic filesystem updates
// and kernel-backed file locks, so independent processes can safely share it.
type FileCache struct {
	directory   string
	maxVersions int
}

// FileCacheOptions configures the default filesystem cache.
type FileCacheOptions struct {
	// Directory is the cache root. Empty uses os.UserCacheDir()/modellink.
	Directory string
	// MaxVersions is the total number of versions to retain. Zero defaults to
	// two; -1 disables automatic cleanup. Positive values must be at least two.
	MaxVersions int
}

// NewFileCache creates a filesystem cache. Zero options use the same defaults
// as New(Options{}).
func NewFileCache(options FileCacheOptions) (*FileCache, error) {
	directory := options.Directory
	if directory == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("modellink: locate user cache directory: %w", err)
		}
		directory = filepath.Join(userCache, "modellink")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("modellink: resolve cache directory: %w", err)
	}
	maxVersions := options.MaxVersions
	if maxVersions == 0 {
		maxVersions = 2
	}
	if maxVersions < -1 || maxVersions == 1 {
		return nil, errors.New("modellink: MaxVersions must be -1 or at least 2")
	}
	return &FileCache{directory: absolute, maxVersions: maxVersions}, nil
}

// Directory returns the absolute cache directory.
func (cache *FileCache) Directory() string {
	if cache == nil {
		return ""
	}
	return cache.directory
}

func (cache *FileCache) Current(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	contents, err := os.ReadFile(filepath.Join(cache.directory, "current.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNoCachedData
		}
		return "", fmt.Errorf("modellink: read current cache version: %w", err)
	}
	var current struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(contents, &current); err != nil {
		return "", fmt.Errorf("modellink: decode current cache version: %w", err)
	}
	if !versionPattern.MatchString(current.Version) {
		return "", errors.New("modellink: current cache version is invalid")
	}
	return current.Version, nil
}

func (cache *FileCache) Get(ctx context.Context, version string) (*CacheEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !versionPattern.MatchString(version) {
		return nil, fmt.Errorf("modellink: invalid cached version %q", version)
	}
	directory := filepath.Join(cache.directory, "versions", version)
	files := make(map[DataFile][]byte, len(cachedFiles))
	for _, name := range cachedFiles {
		contents, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrNoCachedData
			}
			return nil, fmt.Errorf("modellink: read cached %s: %w", name, err)
		}
		files[DataFile(name)] = contents
	}
	entry := &CacheEntry{Version: version, Files: files}
	if _, err := packageFromEntry(entry); err != nil {
		return nil, err
	}
	return entry, nil
}

func (cache *FileCache) Put(ctx context.Context, entry *CacheEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pkg, err := packageFromEntry(entry)
	if err != nil {
		return err
	}
	versions := filepath.Join(cache.directory, "versions")
	if err := os.MkdirAll(versions, 0o700); err != nil {
		return fmt.Errorf("modellink: create cache: %w", err)
	}
	target := filepath.Join(versions, entry.Version)
	if _, err := os.Stat(target); err == nil {
		if _, loadErr := cache.Get(ctx, entry.Version); loadErr == nil {
			return nil
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("modellink: remove corrupt cached package: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("modellink: inspect cache target: %w", err)
	}

	temporary, err := os.MkdirTemp(versions, ".install-*")
	if err != nil {
		return fmt.Errorf("modellink: create cache staging directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	for _, name := range cachedFiles {
		if err := os.WriteFile(filepath.Join(temporary, name), pkg.Files[name], 0o600); err != nil {
			return fmt.Errorf("modellink: write cached %s: %w", name, err)
		}
	}
	if err := os.Rename(temporary, target); err != nil {
		if _, loadErr := cache.Get(ctx, entry.Version); loadErr == nil {
			return nil
		}
		return fmt.Errorf("modellink: activate cached package: %w", err)
	}
	return nil
}

func (cache *FileCache) SetCurrent(ctx context.Context, version string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("modellink: invalid current version %q", version)
	}
	if _, err := cache.Get(ctx, version); err != nil {
		return fmt.Errorf("modellink: activate unavailable version %q: %w", version, err)
	}
	if err := os.MkdirAll(cache.directory, 0o700); err != nil {
		return fmt.Errorf("modellink: create cache directory: %w", err)
	}
	contents, err := json.Marshal(struct {
		Version string `json:"version"`
	}{Version: version})
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(cache.directory, ".current-*")
	if err != nil {
		return fmt.Errorf("modellink: create current version file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filepath.Join(cache.directory, "current.json")); err != nil {
		return fmt.Errorf("modellink: update current version: %w", err)
	}
	return nil
}

func (cache *FileCache) Lock(ctx context.Context, key string) (Lock, error) {
	if key == "" {
		return nil, errors.New("modellink: cache lock key is empty")
	}
	locks := filepath.Join(cache.directory, ".locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		return nil, fmt.Errorf("modellink: create lock directory: %w", err)
	}
	digest := sha256.Sum256([]byte(key))
	path := filepath.Join(locks, hex.EncodeToString(digest[:])+".lock")
	fileLock := flock.New(path)
	locked, err := fileLock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("modellink: acquire cache lock %q: %w", key, err)
	}
	if !locked {
		return nil, fmt.Errorf("modellink: acquire cache lock %q: %w", key, ctx.Err())
	}
	return fileLock, nil
}

// Prune removes old immutable versions while preserving the active version and
// every version named in protected. Busy versions are serialized by their
// version lock before removal.
func (cache *FileCache) Prune(ctx context.Context, protected ...string) error {
	if cache.maxVersions < 0 {
		return nil
	}
	keep := make(map[string]struct{}, len(protected)+1)
	for _, version := range protected {
		if versionPattern.MatchString(version) {
			keep[version] = struct{}{}
		}
	}
	if current, err := cache.Current(ctx); err == nil {
		keep[current] = struct{}{}
	} else if !errors.Is(err, ErrNoCachedData) {
		return err
	}

	entries, err := os.ReadDir(filepath.Join(cache.directory, "versions"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("modellink: list cached versions: %w", err)
	}
	type candidate struct {
		version  string
		modified time.Time
	}
	versions := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !versionPattern.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("modellink: inspect cached version %s: %w", entry.Name(), err)
		}
		versions = append(versions, candidate{version: entry.Name(), modified: info.ModTime()})
	}
	sort.Slice(versions, func(left, right int) bool {
		return versions[left].modified.After(versions[right].modified)
	})
	remaining := cache.maxVersions - len(keep)
	if remaining < 0 {
		remaining = 0
	}
	for _, version := range versions {
		if _, protected := keep[version.version]; protected {
			continue
		}
		if remaining > 0 {
			keep[version.version] = struct{}{}
			remaining--
			continue
		}
		lock, err := cache.Lock(ctx, "version:"+version.version)
		if err != nil {
			return err
		}
		current, currentErr := cache.Current(ctx)
		if currentErr == nil && current == version.version {
			_ = lock.Unlock()
			continue
		}
		if currentErr != nil && !errors.Is(currentErr, ErrNoCachedData) {
			_ = lock.Unlock()
			return currentErr
		}
		removeErr := os.RemoveAll(filepath.Join(cache.directory, "versions", version.version))
		unlockErr := lock.Unlock()
		if removeErr != nil {
			return fmt.Errorf("modellink: remove cached version %s: %w", version.version, removeErr)
		}
		if unlockErr != nil {
			return fmt.Errorf("modellink: unlock cached version %s: %w", version.version, unlockErr)
		}
	}
	return nil
}

func packageFromEntry(entry *CacheEntry) (*artifact.Package, error) {
	if entry == nil {
		return nil, errors.New("modellink: cache entry is nil")
	}
	if !versionPattern.MatchString(entry.Version) {
		return nil, fmt.Errorf("modellink: invalid cached version %q", entry.Version)
	}
	files := make(map[string][]byte, len(cachedFiles))
	for _, name := range cachedFiles {
		contents, ok := entry.Files[DataFile(name)]
		if !ok {
			return nil, fmt.Errorf("modellink: cache entry is missing %s", name)
		}
		files[name] = append([]byte(nil), contents...)
	}
	var manifest artifact.Manifest
	if err := json.Unmarshal(files[string(FileManifest)], &manifest); err != nil {
		return nil, fmt.Errorf("modellink: decode cached manifest: %w", err)
	}
	if manifest.Version != entry.Version {
		return nil, errors.New("modellink: cached manifest version mismatch")
	}
	if err := artifact.VerifyFiles(manifest, files); err != nil {
		return nil, err
	}
	return &artifact.Package{
		Release:  artifact.Release{Version: entry.Version},
		Manifest: manifest,
		Files:    files,
	}, nil
}

func entryFromPackage(pkg *artifact.Package) *CacheEntry {
	files := make(map[DataFile][]byte, len(cachedFiles))
	for _, name := range cachedFiles {
		files[DataFile(name)] = append([]byte(nil), pkg.Files[name]...)
	}
	return &CacheEntry{Version: pkg.Manifest.Version, Files: files}
}

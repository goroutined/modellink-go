package modellink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goroutined/modellink-go/internal/artifact"
	"github.com/goroutined/modellink-go/internal/trace"
)

var cachedFiles = []string{
	string(FileManifest),
	string(FileAPI),
	string(FileModels),
	string(FileCatalog),
	string(FileSchema),
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
	state, err := cache.State(ctx)
	if err != nil {
		return "", err
	}
	if state.Version == "" {
		return "", ErrNoCachedData
	}
	return state.Version, nil
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
	_, verifyEnd := trace.Start(ctx, "verify")
	_, verifyErr := packageFromEntry(entry)
	verifyEnd()
	if err := verifyErr; err != nil {
		return nil, err
	}
	return entry, nil
}

func (cache *FileCache) Put(ctx context.Context, entry *CacheEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, verifyEnd := trace.Start(ctx, "verify")
	pkg, err := packageFromEntry(entry)
	verifyEnd()
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
	readCtx, readEnd := trace.Start(ctx, "cache_read")
	_, readErr := cache.Get(readCtx, version)
	readEnd()
	if err := readErr; err != nil {
		return fmt.Errorf("modellink: activate unavailable version %q: %w", version, err)
	}
	return cache.changeState(ctx, func(state *CacheState) {
		if state.Version != version {
			state.LastUpdate = &UpdateState{UpdatedAt: time.Now().UTC(), PreviousVersion: state.Version, CurrentVersion: version}
			state.Version = version
		}
	})
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

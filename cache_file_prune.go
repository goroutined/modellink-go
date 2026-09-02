package modellink

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

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

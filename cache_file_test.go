package modellink

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestFileCacheLockHonorsContextTimeout(t *testing.T) {
	directory := t.TempDir()
	first := mustFileCache(t, directory)
	second := mustFileCache(t, directory)
	held, err := first.Lock(context.Background(), "latest")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err = second.Lock(ctx, "latest")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock returned %v, want context deadline", err)
	}
}

func TestNewCacheComposesStoreAndLocker(t *testing.T) {
	fileCache := mustFileCache(t, t.TempDir())
	cache, err := NewCache(fileCache, fileCache)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Options{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if client.cache != cache {
		t.Fatal("client did not retain the composed cache")
	}
}

func TestClientReportsCacheLockTimeout(t *testing.T) {
	blocked := &blockedCache{FileCache: mustFileCache(t, t.TempDir())}
	client, err := New(Options{Cache: blocked})
	if err != nil {
		t.Fatal(err)
	}
	client.lockTimeout = 20 * time.Millisecond
	_, err = client.LoadLatest(context.Background())
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("LoadLatest returned %v, want ErrLockTimeout", err)
	}
}

func cachedVersionNames(t testing.TB, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(directory, "versions"))
	if err != nil {
		t.Fatal(err)
	}
	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && versionPattern.MatchString(entry.Name()) {
			versions = append(versions, entry.Name())
		}
	}
	sort.Strings(versions)
	return versions
}

func TestFileCacheRetainsCurrentAndOneRecentVersion(t *testing.T) {
	directory := t.TempDir()
	registry := newTestRegistry(t, "1.0.0", map[string]int{
		"1.0.0": 1,
		"2.0.0": 1,
		"3.0.0": 1,
	})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache: mustConfiguredFileCache(t, FileCacheOptions{
			Directory: directory,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		registry.latest.Store(version)
		if _, err := client.LoadLatest(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	versions := cachedVersionNames(t, directory)
	if strings.Join(versions, ",") != "2.0.0,3.0.0" {
		t.Fatalf("cached versions are %v, want [2.0.0 3.0.0]", versions)
	}
}

func TestFileCacheCanDisableVersionCleanup(t *testing.T) {
	directory := t.TempDir()
	registry := newTestRegistry(t, "1.0.0", map[string]int{
		"1.0.0": 1,
		"2.0.0": 1,
		"3.0.0": 1,
	})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache: mustConfiguredFileCache(t, FileCacheOptions{
			Directory:   directory,
			MaxVersions: -1,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		registry.latest.Store(version)
		if _, err := client.LoadLatest(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	versions := cachedVersionNames(t, directory)
	if strings.Join(versions, ",") != "1.0.0,2.0.0,3.0.0" {
		t.Fatalf("cached versions are %v, want all versions", versions)
	}
}

func TestFileCacheBoundsExplicitVersionsWithoutDeletingCurrent(t *testing.T) {
	directory := t.TempDir()
	registry := newTestRegistry(t, "1.0.0", map[string]int{
		"1.0.0": 1,
		"2.0.0": 1,
		"3.0.0": 1,
	})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadLatest(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"2.0.0", "3.0.0"} {
		if _, err := client.LoadVersion(context.Background(), version); err != nil {
			t.Fatal(err)
		}
	}
	versions := cachedVersionNames(t, directory)
	if strings.Join(versions, ",") != "1.0.0,3.0.0" {
		t.Fatalf("cached versions are %v, want current and newest explicit version", versions)
	}
	current, err := client.cache.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current != "1.0.0" {
		t.Fatalf("explicit version cleanup changed current to %q", current)
	}
}

func TestFileCacheUsesDefaultDirectoryWhenOnlyRetentionIsSet(t *testing.T) {
	cache := mustConfiguredFileCache(t, FileCacheOptions{MaxVersions: 5})
	root, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(filepath.Join(root, "modellink"))
	if err != nil {
		t.Fatal(err)
	}
	if cache.Directory() != want {
		t.Fatalf("default directory is %q, want %q", cache.Directory(), want)
	}
}

func TestFileCacheRejectsOneVersionRetention(t *testing.T) {
	_, err := NewFileCache(FileCacheOptions{Directory: t.TempDir(), MaxVersions: 1})
	if err == nil {
		t.Fatal("expected MaxVersions=1 to be rejected")
	}
}

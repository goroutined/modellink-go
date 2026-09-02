package modellink

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goroutined/modellink-go/internal/artifact"
)

func TestConcurrentLoadLatestDownloadsOnce(t *testing.T) {
	registry := newTestRegistry(t, "1.2.3", map[string]int{"1.2.3": 1})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 24
	results := make(chan *Snapshot, callers)
	errors := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			snapshot, err := client.LoadLatest(context.Background())
			if err != nil {
				errors <- err
				return
			}
			results <- snapshot
		}()
	}
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	var first *Snapshot
	for snapshot := range results {
		if snapshot.Manifest.Version != "1.2.3" {
			t.Fatalf("unexpected version %q", snapshot.Manifest.Version)
		}
		if first == nil {
			first = snapshot
		} else if first != snapshot {
			t.Fatal("concurrent callers did not share the same snapshot")
		}
	}
	if got := registry.downloads.Load(); got != 1 {
		t.Fatalf("downloaded package %d times, want 1", got)
	}
	raw, ok := first.File(FileCatalog)
	if !ok || len(raw) == 0 {
		t.Fatal("verified raw catalog is unavailable")
	}
	raw[0] = 'x'
	again, _ := first.File(FileCatalog)
	if again[0] == 'x' {
		t.Fatal("Snapshot.File exposed mutable internal bytes")
	}

	status, err := client.CheckLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.UpdateAvailable || status.CurrentVersion != "1.2.3" {
		t.Fatalf("unexpected update status: %+v", status)
	}
}

func TestIndependentClientsShareFileCacheAndDownloadOnce(t *testing.T) {
	registry := newTestRegistry(t, "1.2.3", map[string]int{"1.2.3": 1})
	directory := t.TempDir()
	first, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errors := make(chan error, 2)
	for _, client := range []*Client{first, second} {
		go func() {
			<-start
			_, err := client.LoadLatest(context.Background())
			errors <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if got := registry.downloads.Load(); got != 1 {
		t.Fatalf("independent clients downloaded package %d times, want 1", got)
	}
}

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

type blockedCache struct {
	*FileCache
}

func (cache *blockedCache) Lock(ctx context.Context, _ string) (Lock, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCanceledWaiterDoesNotCancelSharedDownload(t *testing.T) {
	registry := newTestRegistry(t, "1.2.3", map[string]int{"1.2.3": 1})
	registry.downloadGate = make(chan struct{})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}

	firstContext, cancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := client.LoadLatest(firstContext)
		firstResult <- err
	}()
	select {
	case <-registry.downloadStarted:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}

	secondResult := make(chan error, 1)
	go func() {
		_, err := client.LoadLatest(context.Background())
		secondResult <- err
	}()
	deadline := time.Now().Add(time.Second)
	for !hasWaiters(client, "latest", 2) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hasWaiters(client, "latest", 2) {
		t.Fatal("second caller did not join the shared download")
	}
	cancel()
	close(registry.downloadGate)
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter returned %v", err)
	}
	if err := <-secondResult; err != nil {
		t.Fatal(err)
	}
	if got := registry.downloads.Load(); got != 1 {
		t.Fatalf("downloaded package %d times, want 1", got)
	}
}

func hasWaiters(client *Client, version string, count int) bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	ongoing := client.flights[version]
	return ongoing != nil && ongoing.waiters >= count
}

func mustFileCache(t testing.TB, directory string) *FileCache {
	t.Helper()
	return mustConfiguredFileCache(t, FileCacheOptions{Directory: directory})
}

func mustConfiguredFileCache(t testing.TB, options FileCacheOptions) *FileCache {
	t.Helper()
	cache, err := NewFileCache(options)
	if err != nil {
		t.Fatal(err)
	}
	return cache
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

func TestLoadUsesVerifiedCacheWithoutNetwork(t *testing.T) {
	cacheDir := t.TempDir()
	registry := newTestRegistry(t, "1.2.3", map[string]int{"1.2.3": 1})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, cacheDir),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadLatest(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry.server.Close()

	offline, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, cacheDir),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := offline.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Manifest.Version != "1.2.3" {
		t.Fatalf("unexpected cached version %q", snapshot.Manifest.Version)
	}
}

func TestLoadRepairsCorruptCachedVersion(t *testing.T) {
	cacheDir := t.TempDir()
	registry := newTestRegistry(t, "1.2.3", map[string]int{"1.2.3": 1})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, cacheDir),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadLatest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cacheDir, "versions", "1.2.3", "catalog.json"),
		[]byte(`{"corrupt":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, cacheDir),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := restarted.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Manifest.Version != "1.2.3" {
		t.Fatal("corrupt cache was not repaired")
	}
	if got := registry.downloads.Load(); got != 2 {
		t.Fatalf("downloaded package %d times, want 2", got)
	}
}

func TestLoadVersionDoesNotChangeActiveVersion(t *testing.T) {
	registry := newTestRegistry(t, "1.0.0", map[string]int{
		"1.0.0": 1,
		"2.0.0": 1,
	})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := client.LoadLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := client.LoadVersion(context.Background(), "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	stillActive, err := client.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active.Manifest.Version != "1.0.0" || fixed.Manifest.Version != "2.0.0" {
		t.Fatal("unexpected loaded versions")
	}
	if stillActive != active {
		t.Fatal("loading a fixed version changed the active snapshot")
	}
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

func TestLoadReturnsActiveSnapshotWhileUpdateIsRunning(t *testing.T) {
	registry := newTestRegistry(t, "1.0.0", map[string]int{
		"1.0.0": 1,
		"2.0.0": 1,
	})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := client.LoadLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	registry.latest.Store("2.0.0")
	registry.downloadGate = make(chan struct{})
	updateResult := make(chan error, 1)
	go func() {
		_, err := client.LoadLatest(context.Background())
		updateResult <- err
	}()
	deadline := time.Now().Add(time.Second)
	for !hasWaiters(client, "latest", 1) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hasWaiters(client, "latest", 1) {
		t.Fatal("update did not start")
	}

	loaded, err := client.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded != active {
		t.Fatal("Load waited for or exposed an incomplete update")
	}
	close(registry.downloadGate)
	if err := <-updateResult; err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedSchemaIsNotActivated(t *testing.T) {
	registry := newTestRegistry(t, "2.0.0", map[string]int{"2.0.0": 2})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.LoadLatest(context.Background())
	if !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("expected unsupported schema error, got %v", err)
	}
	if _, err := client.cache.Current(context.Background()); !errors.Is(err, ErrNoCachedData) {
		t.Fatalf("unsupported package became active: %v", err)
	}
}

func TestInterleavedAcceptsBothPublishedForms(t *testing.T) {
	for _, input := range []string{
		`{"interleaved":true}`,
		`{"interleaved":{"field":"reasoning_content"}}`,
	} {
		var value struct {
			Interleaved *Interleaved `json:"interleaved"`
		}
		if err := json.Unmarshal([]byte(input), &value); err != nil {
			t.Fatal(err)
		}
		if value.Interleaved == nil {
			t.Fatal("interleaved value was not decoded")
		}
	}
}

type testRegistry struct {
	server          *httptest.Server
	metadata        atomic.Int64
	downloads       atomic.Int64
	downloadStarted chan struct{}
	downloadGate    chan struct{}
	downloadOnce    sync.Once
	latest          atomic.Value
}

func newTestRegistry(t *testing.T, latest string, schemaVersions map[string]int) *testRegistry {
	t.Helper()
	type releaseData struct {
		archive   []byte
		integrity string
	}
	releases := make(map[string]releaseData, len(schemaVersions))
	for version, schemaVersion := range schemaVersions {
		archive, integrity := makeTestArchive(t, version, schemaVersion)
		releases[version] = releaseData{archive: archive, integrity: integrity}
	}

	registry := &testRegistry{}
	registry.latest.Store(latest)
	registry.downloadStarted = make(chan struct{})
	registry.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/@modellink/data/latest" {
			registry.metadata.Add(1)
			latestVersion := registry.latest.Load().(string)
			writeMetadata(
				t,
				response,
				registry.server.URL,
				latestVersion,
				releases[latestVersion].integrity,
			)
			return
		}
		if version, ok := strings.CutPrefix(request.URL.Path, "/@modellink/data/"); ok {
			if release, ok := releases[version]; ok {
				registry.metadata.Add(1)
				writeMetadata(t, response, registry.server.URL, version, release.integrity)
				return
			}
		}
		if archiveName, ok := strings.CutPrefix(request.URL.Path, "/tar/"); ok {
			version, hasSuffix := strings.CutSuffix(archiveName, ".tgz")
			if !hasSuffix {
				http.NotFound(response, request)
				return
			}
			if release, ok := releases[version]; ok {
				registry.downloads.Add(1)
				registry.downloadOnce.Do(func() { close(registry.downloadStarted) })
				if registry.downloadGate != nil {
					<-registry.downloadGate
				}
				_, _ = response.Write(release.archive)
				return
			}
		}
		http.NotFound(response, request)
	}))
	t.Cleanup(registry.server.Close)
	return registry
}

func writeMetadata(
	t *testing.T,
	response http.ResponseWriter,
	serverURL string,
	version string,
	integrity string,
) {
	t.Helper()
	if err := json.NewEncoder(response).Encode(map[string]any{
		"version": version,
		"dist": map[string]string{
			"tarball":   serverURL + "/tar/" + version + ".tgz",
			"integrity": integrity,
		},
	}); err != nil {
		t.Error(err)
	}
}

func makeTestArchive(t *testing.T, version string, schemaVersion int) ([]byte, string) {
	t.Helper()
	files := map[string][]byte{
		"api.json":     []byte(`{}`),
		"models.json":  []byte(`{}`),
		"catalog.json": []byte(`{"models":{},"providers":{}}`),
		"schema.json":  []byte(`{}`),
	}
	manifest := artifact.Manifest{
		Version:       version,
		SchemaVersion: schemaVersion,
		GeneratedAt:   "2026-09-01T00:00:00Z",
		Source: artifact.ManifestSource{
			Repository: "https://github.com/goroutined/modellink",
			Revision:   "test",
		},
		Files: make(map[string]artifact.ManifestFile),
	}
	for name, contents := range files {
		digest := sha256.Sum256(contents)
		manifest.Files[name] = artifact.ManifestFile{
			SHA256: hex.EncodeToString(digest[:]),
			Size:   int64(len(contents)),
		}
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	files["manifest.json"] = manifestJSON

	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range cachedFiles {
		contents := files[name]
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: "package/" + name,
			Mode: 0o644,
			Size: int64(len(contents)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha512.Sum512(output.Bytes())
	return output.Bytes(), "sha512-" + base64.StdEncoding.EncodeToString(digest[:])
}

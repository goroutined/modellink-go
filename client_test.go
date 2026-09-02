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
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goroutined/modellink-go/internal/artifact"
)

type blockedCache struct {
	*FileCache
}

func (cache *blockedCache) Lock(ctx context.Context, _ string) (Lock, error) {
	<-ctx.Done()
	return nil, ctx.Err()
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

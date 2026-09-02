package artifact

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
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolverDownloadsAndVerifiesPackage(t *testing.T) {
	archive, integrity := testArchive(t, "1.2.3", 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/@modellink/data/latest":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"version": "1.2.3",
				"dist": map[string]string{
					"tarball":   "http://" + request.Host + "/data.tgz",
					"integrity": integrity,
				},
			})
		case "/data.tgz":
			_, _ = response.Write(archive)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	resolver := Resolver{Registry: server.URL, HTTPClient: server.Client()}
	release, err := resolver.Resolve(context.Background(), "latest")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := resolver.Download(context.Background(), release)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Manifest.Version != "1.2.3" {
		t.Fatalf("unexpected version %q", pkg.Manifest.Version)
	}
	if string(pkg.Files["catalog.json"]) != `{"models":{},"providers":{}}` {
		t.Fatal("unexpected catalog contents")
	}
}

func TestResolverRejectsIntegrityMismatch(t *testing.T) {
	archive, _ := testArchive(t, "1.2.3", 1)
	release := Release{
		Version:   "1.2.3",
		Integrity: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, sha512.Size)),
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(archive)
	}))
	defer server.Close()
	release.Tarball = server.URL

	_, err := (Resolver{HTTPClient: server.Client()}).Download(context.Background(), release)
	if err == nil {
		t.Fatal("expected integrity mismatch")
	}
}

func testArchive(t *testing.T, version string, schemaVersion int) ([]byte, string) {
	t.Helper()
	files := map[string][]byte{
		"api.json":     []byte(`{}`),
		"models.json":  []byte(`{}`),
		"catalog.json": []byte(`{"models":{},"providers":{}}`),
		"schema.json":  []byte(`{}`),
	}
	manifest := Manifest{
		Version:       version,
		SchemaVersion: schemaVersion,
		GeneratedAt:   "2026-09-01T00:00:00Z",
		Source: ManifestSource{
			Repository: "https://github.com/goroutined/modellink",
			Revision:   "test",
		},
		Files: make(map[string]ManifestFile),
	}
	for name, contents := range files {
		digest := sha256.Sum256(contents)
		manifest.Files[name] = ManifestFile{
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
	for _, name := range []string{"manifest.json", "api.json", "models.json", "catalog.json", "schema.json"} {
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

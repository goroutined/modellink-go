package main

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
	"fmt"
	"testing"

	modellink "github.com/goroutined/modellink-go"
)

func TestVerifyPackagesUpgradesFromBaseline(t *testing.T) {
	baseline := makePackage(t, "1.0.0", modellink.SupportedSchemaVersion, []byte(`{"baseline":true}`))
	candidate := makePackage(t, "1.1.0", modellink.SupportedSchemaVersion, []byte(`{"candidate":true}`))

	report, err := verifyPackages(context.Background(), baseline, candidate, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Baseline == nil || report.Baseline.Version != baseline.version {
		t.Fatalf("unexpected baseline report: %+v", report.Baseline)
	}
	if report.Candidate.Version != candidate.version {
		t.Fatalf("unexpected candidate version %q", report.Candidate.Version)
	}
	if report.Candidate.Models != 1 || report.Candidate.Providers != 1 || report.Candidate.Offerings != 1 {
		t.Fatalf("unexpected inventory: %+v", report.Candidate)
	}
}

func TestVerifyPackagesRejectsFutureSchema(t *testing.T) {
	candidate := makePackage(t, "1.1.0", modellink.SupportedSchemaVersion+1, []byte(`{}`))

	_, err := verifyPackages(context.Background(), nil, candidate, false)
	if err == nil {
		t.Fatal("expected future Schema to be rejected")
	}
}

func TestVerifyPackagesStrictSchemaRejectsDrift(t *testing.T) {
	candidate := makePackage(t, "1.1.0", modellink.SupportedSchemaVersion, []byte(`{"changed":true}`))

	_, err := verifyPackages(context.Background(), nil, candidate, true)
	if err == nil {
		t.Fatal("expected strict Schema verification to reject drift")
	}
}

func makePackage(t *testing.T, version string, schemaVersion int, schema []byte) *dataPackage {
	t.Helper()

	modelsJSON := []byte(`{
		"example/model": {"id":"example/model","name":"Example Model"}
	}`)
	providerJSON := []byte(`{
		"example": {
			"id":"example",
			"name":"Example",
			"doc":"https://example.com",
			"npm":"example",
			"protocol":"openai-compatible",
			"api":"https://api.example.com",
			"env":["EXAMPLE_API_KEY"],
			"models":{
				"model":{
					"id":"model",
					"name":"Example Model",
					"description":"Example model",
					"release_date":"2026-01-01",
					"last_updated":"2026-01-01",
					"attachment":false,
					"open_weights":false,
					"limit":{"context":1024,"input":1024,"output":1024},
					"modalities":{"input":["text"],"output":["text"]}
				}
			}
		}
	}`)
	catalogJSON, err := json.Marshal(map[string]any{
		"models":    json.RawMessage(modelsJSON),
		"providers": json.RawMessage(providerJSON),
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"api.json":     providerJSON,
		"models.json":  modelsJSON,
		"catalog.json": catalogJSON,
		"schema.json":  schema,
	}

	manifestFiles := make(map[string]manifestFile, len(files))
	for name, contents := range files {
		digest := sha256.Sum256(contents)
		manifestFiles[name] = manifestFile{
			SHA256: hex.EncodeToString(digest[:]),
			Size:   int64(len(contents)),
		}
	}
	manifestJSON, err := json.Marshal(map[string]any{
		"version":        version,
		"schema_version": schemaVersion,
		"generated_at":   "2026-01-01T00:00:00Z",
		"source": map[string]string{
			"repository": "https://github.com/goroutined/modellink",
			"revision":   "test",
		},
		"files": manifestFiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	files["manifest.json"] = manifestJSON

	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range requiredFiles {
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

	contents := archive.Bytes()
	schemaDigest := sha256.Sum256(schema)
	archiveDigest := sha512.Sum512(contents)
	return &dataPackage{
		tarball:       contents,
		integrity:     "sha512-" + base64.StdEncoding.EncodeToString(archiveDigest[:]),
		version:       version,
		schemaHash:    hex.EncodeToString(schemaDigest[:]),
		schemaVersion: schemaVersion,
		files:         files,
	}
}

func TestRequiredFilesAreStable(t *testing.T) {
	if fmt.Sprint(requiredFiles) != "[api.json models.json catalog.json schema.json manifest.json]" {
		t.Fatalf("unexpected required files: %v", requiredFiles)
	}
}

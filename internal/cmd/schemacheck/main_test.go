package main

import (
	"testing"

	"github.com/goroutined/modellink-go/internal/artifact"
)

func TestSchemasMatchIgnoresDataPackageVersion(t *testing.T) {
	lock := schemaLock{
		PackageVersion: "0.1.4",
		SchemaVersion:  1,
		SchemaSHA256:   "same-schema",
	}
	manifest := artifact.Manifest{
		Version:       "0.1.5",
		SchemaVersion: 1,
		Files: map[string]artifact.ManifestFile{
			"schema.json": {SHA256: "same-schema"},
		},
	}
	if !schemasMatch(lock, manifest) {
		t.Fatal("data-only package update was reported as a Schema change")
	}
}

func TestSchemasMatchDetectsSchemaChanges(t *testing.T) {
	base := schemaLock{SchemaVersion: 1, SchemaSHA256: "schema-one"}
	tests := []struct {
		name     string
		manifest artifact.Manifest
	}{
		{
			name: "compatibility version",
			manifest: artifact.Manifest{
				SchemaVersion: 2,
				Files: map[string]artifact.ManifestFile{
					"schema.json": {SHA256: "schema-two"},
				},
			},
		},
		{
			name: "schema contents",
			manifest: artifact.Manifest{
				SchemaVersion: 1,
				Files: map[string]artifact.ManifestFile{
					"schema.json": {SHA256: "changed-schema"},
				},
			},
		},
		{
			name: "missing schema manifest entry",
			manifest: artifact.Manifest{
				SchemaVersion: 1,
				Files:         map[string]artifact.ManifestFile{},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if schemasMatch(base, test.manifest) {
				t.Fatal("Schema change was not detected")
			}
		})
	}
}

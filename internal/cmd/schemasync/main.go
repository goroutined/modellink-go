package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/goroutined/modellink-go/internal/artifact"
	"github.com/goroutined/modellink-go/internal/atomicfile"
)

func main() {
	registry := flag.String(
		"registry",
		artifact.AuthoritativeRegistry,
		"npm registry used to resolve the data package",
	)
	version := flag.String("version", "latest", "@modellink/data version to copy")
	flag.Parse()

	root, err := os.Getwd()
	check(err)
	client := &http.Client{Timeout: 2 * time.Minute}
	resolver := artifact.Resolver{Registry: *registry, HTTPClient: client}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	release, err := resolver.Resolve(ctx, *version)
	check(err)
	pkg, err := resolver.Download(ctx, release)
	check(err)

	lock := struct {
		Package        string `json:"package"`
		PackageVersion string `json:"package_version"`
		SchemaVersion  int    `json:"schema_version"`
		SchemaSHA256   string `json:"schema_sha256"`
		SourceRevision string `json:"source_revision"`
	}{
		Package:        artifact.PackageName,
		PackageVersion: pkg.Manifest.Version,
		SchemaVersion:  pkg.Manifest.SchemaVersion,
		SchemaSHA256:   pkg.Manifest.Files["schema.json"].SHA256,
		SourceRevision: pkg.Manifest.Source.Revision,
	}
	lockJSON, err := json.MarshalIndent(lock, "", "  ")
	check(err)
	lockJSON = append(lockJSON, '\n')

	directory := filepath.Join(root, "schema")
	check(os.MkdirAll(directory, 0o755))
	check(atomicfile.Write(filepath.Join(directory, "schema.json"), pkg.Files["schema.json"], 0o644))
	check(atomicfile.Write(filepath.Join(directory, "schema.lock.json"), lockJSON, 0o644))
	fmt.Printf(
		"synced %s %s (schema version %d)\n",
		artifact.PackageName,
		pkg.Manifest.Version,
		pkg.Manifest.SchemaVersion,
	)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

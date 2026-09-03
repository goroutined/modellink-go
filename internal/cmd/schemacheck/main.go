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
)

func main() {
	registry := flag.String(
		"registry",
		artifact.AuthoritativeRegistry,
		"npm registry used to check the latest data package",
	)
	flag.Parse()

	root, err := os.Getwd()
	check(err)
	contents, err := os.ReadFile(filepath.Join(root, "schema", "schema.lock.json"))
	check(err)
	var lock schemaLock
	check(json.Unmarshal(contents, &lock))
	if lock.PackageVersion == "" || lock.SchemaVersion < 1 || lock.SchemaSHA256 == "" {
		check(fmt.Errorf("schema lock is incomplete"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resolver := artifact.Resolver{
		Registry:   *registry,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
	release, err := resolver.Resolve(ctx, "latest")
	check(err)
	pkg, err := resolver.Download(ctx, release)
	check(err)
	if schemasMatch(lock, pkg.Manifest) {
		fmt.Printf(
			"schema v%d is current; latest @modellink/data is %s\n",
			lock.SchemaVersion,
			release.Version,
		)
		return
	}
	latestSchema := pkg.Manifest.Files["schema.json"]
	message := fmt.Sprintf(
		"ModelLink schema changed from v%d (%s, @modellink/data %s) to v%d (%s, @modellink/data %s); run `go run ./internal/cmd/schemasync` and `go generate ./...` when ready",
		lock.SchemaVersion,
		shortHash(lock.SchemaSHA256),
		lock.PackageVersion,
		pkg.Manifest.SchemaVersion,
		shortHash(latestSchema.SHA256),
		pkg.Manifest.Version,
	)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		fmt.Printf("::warning title=ModelLink schema update available::%s\n", message)
	} else {
		fmt.Println(message)
	}
}

type schemaLock struct {
	PackageVersion string `json:"package_version"`
	SchemaVersion  int    `json:"schema_version"`
	SchemaSHA256   string `json:"schema_sha256"`
}

func schemasMatch(lock schemaLock, manifest artifact.Manifest) bool {
	latest, ok := manifest.Files["schema.json"]
	return ok &&
		lock.SchemaVersion == manifest.SchemaVersion &&
		lock.SchemaSHA256 == latest.SHA256
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

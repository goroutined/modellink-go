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
	var lock struct {
		PackageVersion string `json:"package_version"`
	}
	check(json.Unmarshal(contents, &lock))
	if lock.PackageVersion == "" {
		check(fmt.Errorf("schema lock has no package_version"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resolver := artifact.Resolver{
		Registry:   *registry,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
	release, err := resolver.Resolve(ctx, "latest")
	check(err)
	if release.Version == lock.PackageVersion {
		fmt.Printf("schema is current at @modellink/data %s\n", release.Version)
		return
	}
	message := fmt.Sprintf(
		"ModelLink schema is based on @modellink/data %s; latest is %s; run `go run ./internal/cmd/schemasync` and `go generate ./...` when ready",
		lock.PackageVersion,
		release.Version,
	)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		fmt.Printf("::warning title=ModelLink schema update available::%s\n", message)
	} else {
		fmt.Println(message)
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

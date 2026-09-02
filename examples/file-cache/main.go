package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	modellink "github.com/goroutined/modellink-go"
)

func main() {
	root, err := os.UserCacheDir()
	if err != nil {
		log.Fatal(err)
	}
	cache, err := modellink.NewFileCache(modellink.FileCacheOptions{
		Directory:   filepath.Join(root, "my-service", "modellink"),
		MaxVersions: 5,
	})
	if err != nil {
		log.Fatal(err)
	}
	client, err := modellink.New(modellink.Options{Cache: cache})
	if err != nil {
		log.Fatal(err)
	}
	snapshot, err := client.Load(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("cache=%s data=%s\n", cache.Directory(), snapshot.Manifest.Version)
}

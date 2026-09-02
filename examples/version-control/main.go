package main

import (
	"context"
	"fmt"
	"log"

	modellink "github.com/goroutined/modellink-go"
)

func main() {
	ctx := context.Background()
	client, err := modellink.New(modellink.Options{})
	if err != nil {
		log.Fatal(err)
	}
	version, err := client.FindLatest(ctx)
	if err != nil {
		log.Fatal(err)
	}
	candidate, err := client.LoadVersion(ctx, version)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("verified candidate: %s\n", candidate.Manifest.Version)

	if err := client.ActivateVersion(ctx, version); err != nil {
		log.Fatal(err)
	}
	active, err := client.LoadCached(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("active data: %s\n", active.Manifest.Version)
}

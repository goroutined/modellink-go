package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"

	modellink "github.com/goroutined/modellink-go"
)

func main() {
	ctx := context.Background()
	client, err := modellink.New(modellink.Options{
		OnWarning: func(warning modellink.Warning) {
			slog.Warn(warning.Message, "code", warning.Code)
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	snapshot, err := client.Load(ctx)
	if err != nil {
		log.Fatal(err)
	}
	status, err := client.CheckLatest(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if status.UpdateAvailable {
		snapshot, err = client.LoadLatest(ctx)
		if err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("active ModelLink data: %s\n", snapshot.Manifest.Version)
}

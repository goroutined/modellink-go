package main

import (
	"context"
	"fmt"
	"log"
	"time"

	modellink "github.com/goroutined/modellink-go"
)

func main() {
	client, err := modellink.New(modellink.Options{
		OnOperation: func(report modellink.OperationReport) {
			log.Printf("%s duration=%s err=%v stages=%v", report.Operation, report.Duration, report.Err, report.Stages)
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	check, err := client.CheckLatest(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("checked=%s latest=%s update=%v duration=%s\n", check.CheckedAt, check.LatestVersion, check.UpdateAvailable, check.Duration)
	if _, err := client.LoadLatest(ctx); err != nil {
		log.Fatal(err)
	}
	state, err := client.Status(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("current=%s lastCheck=%+v lastUpdate=%+v\n", state.Version, state.LastCheck, state.LastUpdate)
}

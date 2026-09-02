package main

import (
	"context"
	"fmt"
	"log"

	modellink "github.com/goroutined/modellink-go"
)

func main() {
	client, err := modellink.New(modellink.Options{})
	if err != nil {
		log.Fatal(err)
	}
	snapshot, err := client.Load(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	model, ok := snapshot.Offering("deepseek", "deepseek-v4-pro")
	if !ok {
		log.Fatal("model not found")
	}
	fmt.Printf(
		"data=%s model=%s context=%d\n",
		snapshot.Manifest.Version,
		model.Name,
		model.Limit.Context,
	)
}

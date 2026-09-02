package modellink

import (
	"context"
	"os"
	"testing"
)

func TestLivePackage(t *testing.T) {
	if os.Getenv("MODELLINK_LIVE_TEST") != "1" {
		t.Skip("set MODELLINK_LIVE_TEST=1 to download the current public package")
	}
	client, err := New(Options{Cache: mustFileCache(t, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.LoadLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Catalog.Models) == 0 || len(snapshot.Catalog.Providers) == 0 {
		t.Fatal("public catalog is empty")
	}
	if _, ok := snapshot.ProviderModel("deepseek", "deepseek-v4-pro"); !ok {
		t.Fatal("expected DeepSeek offering is missing")
	}
}

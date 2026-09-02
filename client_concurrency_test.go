package modellink

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestConcurrentLoadLatestDownloadsOnce(t *testing.T) {
	registry := newTestRegistry(t, "1.2.3", map[string]int{"1.2.3": 1})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 24
	results := make(chan *Snapshot, callers)
	errors := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			snapshot, err := client.LoadLatest(context.Background())
			if err != nil {
				errors <- err
				return
			}
			results <- snapshot
		}()
	}
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	var first *Snapshot
	for snapshot := range results {
		if snapshot.Manifest.Version != "1.2.3" {
			t.Fatalf("unexpected version %q", snapshot.Manifest.Version)
		}
		if first == nil {
			first = snapshot
		} else if first != snapshot {
			t.Fatal("concurrent callers did not share the same snapshot")
		}
	}
	if got := registry.downloads.Load(); got != 1 {
		t.Fatalf("downloaded package %d times, want 1", got)
	}
	raw, ok := first.File(FileCatalog)
	if !ok || len(raw) == 0 {
		t.Fatal("verified raw catalog is unavailable")
	}
	raw[0] = 'x'
	again, _ := first.File(FileCatalog)
	if again[0] == 'x' {
		t.Fatal("Snapshot.File exposed mutable internal bytes")
	}

	status, err := client.CheckLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.UpdateAvailable || status.CurrentVersion != "1.2.3" {
		t.Fatalf("unexpected update status: %+v", status)
	}
}

func TestLoadLatestPreventsRegistryDowngrade(t *testing.T) {
	registry := newTestRegistry(t, "2.0.0", map[string]int{
		"1.0.0": 1,
		"2.0.0": 1,
	})
	var mutex sync.Mutex
	var warnings []Warning
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, t.TempDir()),
		OnWarning: func(warning Warning) {
			mutex.Lock()
			warnings = append(warnings, warning)
			mutex.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := client.LoadLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active.Manifest.Version != "2.0.0" {
		t.Fatal("initial latest version was not activated")
	}
	downloadsBefore := registry.downloads.Load()
	registry.latest.Store("1.0.0")

	status, err := client.CheckLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.UpdateAvailable || !status.RegistryBehind ||
		status.CurrentVersion != "2.0.0" || status.LatestVersion != "1.0.0" {
		t.Fatalf("unexpected behind status: %+v", status)
	}
	loaded, err := client.LoadLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Manifest.Version != "2.0.0" {
		t.Fatalf("LoadLatest downgraded to %q", loaded.Manifest.Version)
	}
	if registry.downloads.Load() != downloadsBefore {
		t.Fatal("LoadLatest downloaded the registry's older version")
	}
	mutex.Lock()
	defer mutex.Unlock()
	var behind []Warning
	for _, warning := range warnings {
		if warning.Code == WarningRegistryBehind {
			behind = append(behind, warning)
		}
	}
	if len(behind) != 1 || behind[0].CurrentVersion != "2.0.0" || behind[0].RegistryVersion != "1.0.0" {
		t.Fatalf("registry warning is incomplete: %+v", behind)
	}
}

func TestIndependentClientsShareFileCacheAndDownloadOnce(t *testing.T) {
	registry := newTestRegistry(t, "1.2.3", map[string]int{"1.2.3": 1})
	directory := t.TempDir()
	first, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errors := make(chan error, 2)
	for _, client := range []*Client{first, second} {
		go func() {
			<-start
			_, err := client.LoadLatest(context.Background())
			errors <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if got := registry.downloads.Load(); got != 1 {
		t.Fatalf("independent clients downloaded package %d times, want 1", got)
	}
}

func TestCanceledWaiterDoesNotCancelSharedDownload(t *testing.T) {
	registry := newTestRegistry(t, "1.2.3", map[string]int{"1.2.3": 1})
	registry.downloadGate = make(chan struct{})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}

	firstContext, cancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := client.LoadLatest(firstContext)
		firstResult <- err
	}()
	select {
	case <-registry.downloadStarted:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}

	secondResult := make(chan error, 1)
	go func() {
		_, err := client.LoadLatest(context.Background())
		secondResult <- err
	}()
	deadline := time.Now().Add(time.Second)
	for !hasWaiters(client, "latest", 2) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hasWaiters(client, "latest", 2) {
		t.Fatal("second caller did not join the shared download")
	}
	cancel()
	close(registry.downloadGate)
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter returned %v", err)
	}
	if err := <-secondResult; err != nil {
		t.Fatal(err)
	}
	if got := registry.downloads.Load(); got != 1 {
		t.Fatalf("downloaded package %d times, want 1", got)
	}
}

func hasWaiters(client *Client, version string, count int) bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	ongoing := client.flights[version]
	return ongoing != nil && ongoing.waiters >= count
}

func TestLoadReturnsActiveSnapshotWhileUpdateIsRunning(t *testing.T) {
	registry := newTestRegistry(t, "1.0.0", map[string]int{
		"1.0.0": 1,
		"2.0.0": 1,
	})
	client, err := New(Options{
		Registry: registry.server.URL,
		Cache:    mustFileCache(t, t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := client.LoadLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	registry.latest.Store("2.0.0")
	registry.downloadGate = make(chan struct{})
	updateResult := make(chan error, 1)
	go func() {
		_, err := client.LoadLatest(context.Background())
		updateResult <- err
	}()
	deadline := time.Now().Add(time.Second)
	for !hasWaiters(client, "latest", 1) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hasWaiters(client, "latest", 1) {
		t.Fatal("update did not start")
	}

	loaded, err := client.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded != active {
		t.Fatal("Load waited for or exposed an incomplete update")
	}
	close(registry.downloadGate)
	if err := <-updateResult; err != nil {
		t.Fatal(err)
	}
}

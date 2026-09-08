package modellink

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestPersistentStateAcrossAllOperations(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t, "2.0.0", map[string]int{"1.0.0": 1, "2.0.0": 1})
	dir := t.TempDir()
	cache := mustFileCache(t, dir)
	client, err := New(Options{Registry: registry.server.URL, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.Status(ctx)
	if err != nil || state.LastCheck != nil || state.LastUpdate != nil {
		t.Fatalf("initial state: %+v %v", state, err)
	}
	if _, err := client.FindLatest(ctx); err != nil {
		t.Fatal(err)
	}
	state, _ = cache.State(ctx)
	if state.LastCheck == nil || state.LastCheck.LatestVersion != "2.0.0" || state.LastCheck.Registry != registry.server.URL || state.Version != "" {
		t.Fatalf("check: %+v", state)
	}
	check := *state.LastCheck
	if _, err := client.LoadVersion(ctx, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	state, _ = cache.State(ctx)
	if !reflect.DeepEqual(state.LastCheck, &check) || state.LastUpdate != nil {
		t.Fatal("fixed load changed check or activation")
	}
	if err := client.ActivateVersion(ctx, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	state, _ = cache.State(ctx)
	if state.LastUpdate == nil || state.LastUpdate.PreviousVersion != "" || state.LastUpdate.CurrentVersion != "1.0.0" {
		t.Fatalf("activation: %+v", state)
	}
	initialUpdate := *state.LastUpdate
	if err := client.ActivateVersion(ctx, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadCached(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CurrentVersion(ctx); err != nil {
		t.Fatal(err)
	}
	state, _ = cache.State(ctx)
	if !reflect.DeepEqual(state.LastUpdate, &initialUpdate) || !reflect.DeepEqual(state.LastCheck, &check) {
		t.Fatal("cache reads/same activation changed metadata")
	}
	if _, err := client.LoadLatest(ctx); err != nil {
		t.Fatal(err)
	}
	state, _ = cache.State(ctx)
	if state.LastUpdate.PreviousVersion != "1.0.0" || state.LastUpdate.CurrentVersion != "2.0.0" {
		t.Fatal("missing upgrade")
	}
	upgrade := *state.LastUpdate
	status, err := client.CheckLatest(ctx)
	if err != nil || status.UpdateAvailable || status.CheckedAt.IsZero() || status.Duration <= 0 {
		t.Fatalf("no update check: %+v %v", status, err)
	}
	registry.latest.Store("1.0.0")
	if _, err := client.LoadLatest(ctx); err != nil {
		t.Fatal(err)
	}
	state, _ = cache.State(ctx)
	if state.LastCheck.LatestVersion != "1.0.0" || !reflect.DeepEqual(state.LastUpdate, &upgrade) {
		t.Fatal("behind registry changed activation")
	}
	if err := client.SwitchVersion(ctx, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	state, _ = cache.State(ctx)
	if state.LastUpdate.PreviousVersion != "2.0.0" || state.LastUpdate.CurrentVersion != "1.0.0" {
		t.Fatal("missing explicit downgrade")
	}
	restarted := mustFileCache(t, dir)
	restored, err := restarted.State(ctx)
	if err != nil || !reflect.DeepEqual(state, restored) {
		t.Fatalf("state lost after reopening: %v", err)
	}
	registry.server.Close()
	if _, err := client.CheckLatest(ctx); err == nil {
		t.Fatal("offline check succeeded")
	}
	after, _ := cache.State(ctx)
	if !reflect.DeepEqual(state, after) {
		t.Fatal("failed check overwrote successful record")
	}
}

func TestLegacyMetadataAndConcurrentMerges(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "current.json"), []byte(`{"version":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cache := mustFileCache(t, dir)
	ctx := context.Background()
	state, err := cache.State(ctx)
	if err != nil || state.Version != "1.0.0" || state.LastUpdate != nil {
		t.Fatal("legacy migration fabricated history")
	}
	start := time.Now().UTC()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := mustFileCache(t, dir)
			if err := c.RecordCheck(ctx, CheckState{CheckedAt: start.Add(time.Duration(i) * time.Second), LatestVersion: "2.0.0", Registry: "https://example.com"}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	state, err = cache.State(ctx)
	if err != nil || state.Version != "1.0.0" || state.LastUpdate != nil || !state.LastCheck.CheckedAt.Equal(start.Add(19*time.Second)) {
		t.Fatalf("lost metadata: %+v %v", state, err)
	}
	lock, err := cache.Lock(ctx, "metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := cache.RecordCheck(short, *state.LastCheck); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock timeout: %v", err)
	}
}

func TestStateProcessHelper(t *testing.T) {
	dir := os.Getenv("MODELLINK_STATE_TEST_DIR")
	if dir == "" {
		return
	}
	cache := mustFileCache(t, dir)
	for i := 0; i < 10; i++ {
		if err := cache.RecordCheck(context.Background(), CheckState{CheckedAt: time.Now().UTC(), LatestVersion: "2.0.0", Registry: "https://example.com"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMetadataCrossProcessMerge(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	cache := mustFileCache(t, dir)
	registry := newTestRegistry(t, "1.0.0", map[string]int{"1.0.0": 1})
	client, err := New(Options{Registry: registry.server.URL, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadVersion(ctx, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStateProcessHelper$")
	cmd.Env = append(os.Environ(), "MODELLINK_STATE_TEST_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := cache.SetCurrent(ctx, "1.0.0"); err != nil {
			t.Error(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	s, err := cache.State(ctx)
	if err != nil || s.Version != "1.0.0" || s.LastCheck == nil || s.LastUpdate == nil {
		t.Fatalf("lost concurrent state: %+v %v", s, err)
	}
}

func awaitReport(t *testing.T, ch <-chan OperationReport) OperationReport {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("missing operation report")
		return OperationReport{}
	}
}
func hasStage(r OperationReport, s Stage) bool {
	for _, v := range r.Stages {
		if v.Stage == s {
			return true
		}
	}
	return false
}

func TestReportsSeparateTransferAndCacheHits(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t, "1.0.0", map[string]int{"1.0.0": 1})
	reports := make(chan OperationReport, 10)
	client, err := New(Options{Registry: registry.server.URL, Cache: mustFileCache(t, t.TempDir()), OnOperation: func(r OperationReport) { reports <- r }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadLatest(ctx); err != nil {
		t.Fatal(err)
	}
	r := awaitReport(t, reports)
	for _, stage := range []Stage{StageResolve, StageDownload, StageVerify, StageCacheWrite, StageActivate, StageLockWait, StagePrune} {
		if !hasStage(r, stage) {
			t.Errorf("missing %s: %+v", stage, r)
		}
	}
	var total time.Duration
	for _, s := range r.Stages {
		if s.Duration < 0 {
			t.Fatal("negative stage")
		}
		total += s.Duration
	}
	if total > r.Duration || r.Duration <= 0 || r.Err != nil || r.FinishedAt.Before(r.StartedAt) {
		t.Fatalf("bad timings: %+v", r)
	}
	if _, err := client.LoadVersion(ctx, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	r = awaitReport(t, reports)
	if hasStage(r, StageDownload) || hasStage(r, StageActivate) || r.Operation != OperationLoadVersion {
		t.Fatalf("cache hit reported network: %+v", r)
	}
	if _, err := client.LoadVersion(ctx, "invalid"); err == nil {
		t.Fatal("invalid version accepted")
	}
	r = awaitReport(t, reports)
	if r.Err == nil {
		t.Fatal("missing error report")
	}
}

func TestSharedReportSurvivesOwnerCancellation(t *testing.T) {
	registry := newTestRegistry(t, "1.0.0", map[string]int{"1.0.0": 1})
	gate := make(chan struct{})
	registry.downloadGate = gate
	var once sync.Once
	defer once.Do(func() { close(gate) })
	reports := make(chan OperationReport, 4)
	client, err := New(Options{Registry: registry.server.URL, Cache: mustFileCache(t, t.TempDir()), OnOperation: func(r OperationReport) { reports <- r }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := make(chan error, 1)
	go func() { _, e := client.LoadLatest(ctx); owner <- e }()
	<-registry.downloadStarted
	cancel()
	if err := <-owner; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer waitCancel()
	if _, err := client.LoadLatest(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter: %v", err)
	}
	r := awaitReport(t, reports)
	if !hasStage(r, StageSharedWait) || hasStage(r, StageDownload) || r.Err == nil {
		t.Fatalf("waiter attributed download: %+v", r)
	}
	once.Do(func() { close(gate) })
	r = awaitReport(t, reports)
	if r.Err != nil || !hasStage(r, StageDownload) {
		t.Fatalf("owner task did not complete: %+v", r)
	}
}

func TestFailedVerificationKeepsSuccessfulCheck(t *testing.T) {
	registry := newTestRegistry(t, "1.0.0", map[string]int{"1.0.0": SupportedSchemaVersion + 1})
	cache := mustFileCache(t, t.TempDir())
	client, err := New(Options{Registry: registry.server.URL, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Load(context.Background()); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("load: %v", err)
	}
	s, err := cache.State(context.Background())
	if err != nil || s.LastCheck == nil || s.LastUpdate != nil || s.Version != "" {
		t.Fatalf("failed install metadata: %+v %v", s, err)
	}
}

type failedStateCache struct{ *FileCache }

func (cache *failedStateCache) RecordCheck(context.Context, CheckState) error {
	return errors.New("state store unavailable")
}

func TestCheckStateWriteFailureIsVisible(t *testing.T) {
	registry := newTestRegistry(t, "1.0.0", map[string]int{"1.0.0": 1})
	cache := &failedStateCache{mustFileCache(t, t.TempDir())}
	client, err := New(Options{Registry: registry.server.URL, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.CheckLatest(context.Background())
	if err == nil || status.CheckedAt.IsZero() || status.LatestVersion != "1.0.0" {
		t.Fatalf("lost partial check: %+v %v", status, err)
	}
	s, _ := cache.State(context.Background())
	if s.LastCheck != nil || s.LastUpdate != nil {
		t.Fatal("failed write fabricated state")
	}
}

type reportTransport func(*http.Request) (*http.Response, error)

func (f reportTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNetworkDownloadFailureKeepsCheckAndReportsAttempt(t *testing.T) {
	registry := newTestRegistry(t, "1.0.0", map[string]int{"1.0.0": 1})
	cache := mustFileCache(t, t.TempDir())
	reports := make(chan OperationReport, 1)
	client, err := New(Options{Registry: registry.server.URL, Cache: cache, OnOperation: func(r OperationReport) { reports <- r }})
	if err != nil {
		t.Fatal(err)
	}
	client.resolver.HTTPClient = &http.Client{Transport: reportTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/tar/1.0.0.tgz" {
			return nil, errors.New("download unavailable")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	if _, err := client.LoadLatest(context.Background()); err == nil {
		t.Fatal("download should fail")
	}
	r := awaitReport(t, reports)
	if r.Err == nil || !hasStage(r, StageDownload) || hasStage(r, StageActivate) {
		t.Fatalf("bad failed report: %+v", r)
	}
	s, err := cache.State(context.Background())
	if err != nil || s.LastCheck == nil || s.LastUpdate != nil {
		t.Fatalf("lost successful check: %+v %v", s, err)
	}
}

func TestEveryPublicOperationReports(t *testing.T) {
	registry := newTestRegistry(t, "1.0.0", map[string]int{"1.0.0": 1})
	reports := make(chan OperationReport, 16)
	cache := mustFileCache(t, t.TempDir())
	client, err := New(Options{Registry: registry.server.URL, Cache: cache, OnOperation: func(r OperationReport) {
		// Acquiring the metadata lock here would time out if callbacks ran under it.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		lock, e := cache.Lock(ctx, "metadata")
		if e != nil {
			t.Error(e)
		} else {
			_ = lock.Unlock()
		}
		reports <- r
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	steps := []struct {
		op  Operation
		run func() error
	}{
		{OperationLoad, func() error { _, e := client.Load(ctx); return e }},
		{OperationLoadLatest, func() error { _, e := client.LoadLatest(ctx); return e }},
		{OperationLoadCached, func() error { _, e := client.LoadCached(ctx); return e }},
		{OperationLoadVersion, func() error { _, e := client.LoadVersion(ctx, "1.0.0"); return e }},
		{OperationFindLatest, func() error { _, e := client.FindLatest(ctx); return e }},
		{OperationCheckLatest, func() error { _, e := client.CheckLatest(ctx); return e }},
		{OperationActivateVersion, func() error { return client.ActivateVersion(ctx, "1.0.0") }},
		{OperationSwitchVersion, func() error { return client.SwitchVersion(ctx, "1.0.0") }},
		{OperationCurrentVersion, func() error { _, e := client.CurrentVersion(ctx); return e }},
		{OperationStatus, func() error { _, e := client.Status(ctx); return e }},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			t.Fatal(err)
		}
		r := awaitReport(t, reports)
		if r.Operation != step.op || r.Err != nil {
			t.Fatalf("report mismatch: %+v", r)
		}
	}
	select {
	case r := <-reports:
		t.Fatalf("duplicate report: %+v", r)
	default:
	}
}

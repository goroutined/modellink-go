package modellink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/goroutined/modellink-go/internal/artifact"
)

// Latest queries the registry without downloading the package.
func (client *Client) Latest(ctx context.Context) (Release, error) {
	release, err := client.resolver.Resolve(ctx, "latest")
	if err != nil {
		return Release{}, err
	}
	return publicRelease(release), nil
}

// CheckLatest compares the active version with the registry latest version.
func (client *Client) CheckLatest(ctx context.Context) (UpdateStatus, error) {
	release, err := client.Latest(ctx)
	if err != nil {
		return UpdateStatus{}, err
	}
	current := client.currentVersion()
	if current == "" {
		current, _ = client.cache.Current(ctx)
	}
	return UpdateStatus{CurrentVersion: current, LatestVersion: release.Version, UpdateAvailable: current != release.Version}, nil
}

// LoadLatest checks the configured registry, downloads the latest release when
// necessary, and atomically makes it the active snapshot.
func (client *Client) LoadLatest(ctx context.Context) (*Snapshot, error) {
	snapshot, err := client.joinFlight(ctx, "latest", client.updateLatest)
	return client.observe(snapshot, err)
}

func (client *Client) joinFlight(ctx context.Context, key string, operation func(context.Context) (*Snapshot, error)) (*Snapshot, error) {
	client.mu.Lock()
	ongoing := client.flights[key]
	if ongoing == nil {
		ongoing = &flight{done: make(chan struct{})}
		client.flights[key] = ongoing
		go client.runFlight(key, ongoing, operation)
	}
	ongoing.waiters++
	client.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ongoing.done:
		return ongoing.snapshot, ongoing.err
	}
}

func (client *Client) runFlight(key string, ongoing *flight, operation func(context.Context) (*Snapshot, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), client.operationTimeout)
	defer cancel()
	snapshot, err := operation(ctx)
	client.mu.Lock()
	ongoing.snapshot, ongoing.err = snapshot, err
	delete(client.flights, key)
	close(ongoing.done)
	client.mu.Unlock()
}

func (client *Client) updateLatest(ctx context.Context) (*Snapshot, error) {
	lock, err := client.acquireLock(ctx, "latest")
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	release, err := client.resolver.Resolve(ctx, "latest")
	if err != nil {
		return nil, err
	}
	maintenance, err := client.acquireLock(ctx, "maintenance")
	if err != nil {
		return nil, err
	}
	snapshot, err := client.loadResolvedVersion(ctx, release)
	if err != nil {
		_ = maintenance.Unlock()
		return nil, err
	}
	if err := client.pruneCache(ctx, snapshot.Manifest.Version); err != nil {
		_ = maintenance.Unlock()
		return nil, err
	}
	activationLock, err := client.acquireLock(ctx, "version:"+snapshot.Manifest.Version)
	if err != nil {
		_ = maintenance.Unlock()
		return nil, err
	}
	if err := client.cache.SetCurrent(ctx, snapshot.Manifest.Version); err != nil {
		_ = activationLock.Unlock()
		_ = maintenance.Unlock()
		return nil, err
	}
	if err := activationLock.Unlock(); err != nil {
		_ = maintenance.Unlock()
		return nil, err
	}
	if err := maintenance.Unlock(); err != nil {
		return nil, err
	}
	client.setCurrent(snapshot)
	return snapshot, nil
}

func (client *Client) pruneCache(ctx context.Context, protected ...string) error {
	pruner, ok := client.cache.(CachePruner)
	if !ok {
		return nil
	}
	return pruner.Prune(ctx, protected...)
}

func (client *Client) downloadAndStore(ctx context.Context, release artifact.Release) (*Snapshot, error) {
	pkg, err := client.resolver.Download(ctx, release)
	if err != nil {
		return nil, err
	}
	snapshot, err := snapshotFromPackage(pkg)
	if err != nil {
		return nil, err
	}
	if err := client.cache.Put(ctx, entryFromPackage(pkg)); err != nil {
		return nil, err
	}
	client.mu.Lock()
	client.snapshots[release.Version] = snapshot
	client.mu.Unlock()
	return snapshot, nil
}

func (client *Client) acquireLock(ctx context.Context, key string) (Lock, error) {
	lockContext, cancel := context.WithTimeout(ctx, client.lockTimeout)
	defer cancel()
	lock, err := client.cache.Lock(lockContext, key)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("%w: %s", ErrLockTimeout, key)
		}
		return nil, err
	}
	return lock, nil
}

func snapshotFromPackage(pkg *artifact.Package) (*Snapshot, error) {
	if pkg.Manifest.SchemaVersion < 1 || pkg.Manifest.SchemaVersion > SupportedSchemaVersion {
		return nil, fmt.Errorf("%w: package uses %d, client supports up to %d", ErrUnsupportedSchema, pkg.Manifest.SchemaVersion, SupportedSchemaVersion)
	}
	var manifest Manifest
	if err := json.Unmarshal(pkg.Files["manifest.json"], &manifest); err != nil {
		return nil, fmt.Errorf("modellink: decode public manifest: %w", err)
	}
	var catalog Catalog
	if err := json.Unmarshal(pkg.Files["catalog.json"], &catalog); err != nil {
		return nil, fmt.Errorf("modellink: decode catalog: %w", err)
	}
	return &Snapshot{
		Manifest: manifest,
		Catalog:  catalog,
		files:    snapshotFiles(pkg.Files),
		warnings: schemaWarnings(manifest),
	}, nil
}

func publicRelease(release artifact.Release) Release {
	return Release{Version: release.Version, Tarball: release.Tarball, Integrity: release.Integrity}
}

func snapshotFiles(files map[string][]byte) map[DataFile][]byte {
	result := make(map[DataFile][]byte, len(cachedFiles))
	for _, name := range cachedFiles {
		if contents, ok := files[name]; ok {
			result[DataFile(name)] = append([]byte(nil), contents...)
		}
	}
	return result
}

package modellink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/goroutined/modellink-go/internal/artifact"
)

func (client *Client) findLatest(ctx context.Context) (string, error) {
	release, err := client.resolver.Resolve(ctx, "latest")
	if err != nil {
		return "", err
	}
	if err := validateVersion(release.Version); err != nil {
		return "", err
	}
	return release.Version, nil
}

func (client *Client) checkLatest(ctx context.Context) (UpdateStatus, error) {
	latest, err := client.findLatest(ctx)
	if err != nil {
		return UpdateStatus{}, err
	}
	current, err := client.readCurrentVersion(ctx)
	if errors.Is(err, ErrNoCachedData) {
		return UpdateStatus{LatestVersion: latest, UpdateAvailable: true}, nil
	}
	if err != nil {
		return UpdateStatus{}, err
	}
	comparison := comparePackageVersions(latest, current)
	return UpdateStatus{
		CurrentVersion:  current,
		LatestVersion:   latest,
		UpdateAvailable: comparison > 0,
		RegistryBehind:  comparison < 0,
	}, nil
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

func (client *Client) loadLatest(ctx context.Context) (*Snapshot, error) {
	return client.joinFlight(ctx, "latest", client.loadLatestOperation)
}

func (client *Client) loadLatestOperation(ctx context.Context) (*Snapshot, error) {
	lock, err := client.acquireLock(ctx, "latest")
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	status, err := client.checkLatest(ctx)
	if err != nil {
		return nil, err
	}
	if status.RegistryBehind {
		client.notifyWarning(registryBehindWarning(status.CurrentVersion, status.LatestVersion))
		return client.loadCachedCurrent(ctx)
	}
	return client.switchVersionOperation(ctx, status.LatestVersion)
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

func snapshotFiles(files map[string][]byte) map[DataFile][]byte {
	result := make(map[DataFile][]byte, len(cachedFiles))
	for _, name := range cachedFiles {
		if contents, ok := files[name]; ok {
			result[DataFile(name)] = append([]byte(nil), contents...)
		}
	}
	return result
}

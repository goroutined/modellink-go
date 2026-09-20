package modellink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goroutined/modellink-go/internal/artifact"
	"github.com/goroutined/modellink-go/internal/trace"
)

func (client *Client) findLatest(ctx context.Context) (string, error) {
	check, err := client.findLatestCheck(ctx)
	return check.LatestVersion, err
}

func (client *Client) findLatestCheck(ctx context.Context) (CheckState, error) {
	release, err := client.resolver.Resolve(ctx, "latest")
	if err != nil {
		return CheckState{}, err
	}
	if err := validateVersion(release.Version); err != nil {
		return CheckState{}, err
	}
	registry := strings.TrimRight(client.resolver.Registry, "/")
	if registry == "" {
		registry = artifact.DefaultRegistry
	}
	check := CheckState{CheckedAt: time.Now().UTC(), LatestVersion: release.Version, Registry: registry}
	ctx, end := trace.Start(ctx, "cache_write")
	defer end()
	if err := client.cache.RecordCheck(ctx, check); err != nil {
		return check, fmt.Errorf("modellink: persist latest check: %w", err)
	}
	return check, nil
}

func (client *Client) checkLatest(ctx context.Context) (UpdateStatus, error) {
	check, err := client.findLatestCheck(ctx)
	latest := check.LatestVersion
	if err != nil {
		return UpdateStatus{LatestVersion: latest, CheckedAt: check.CheckedAt}, err
	}
	current, err := client.readCurrentVersion(ctx)
	if errors.Is(err, ErrNoCachedData) {
		return UpdateStatus{LatestVersion: latest, CheckedAt: check.CheckedAt, UpdateAvailable: true}, nil
	}
	if err != nil {
		return UpdateStatus{LatestVersion: latest, CheckedAt: check.CheckedAt}, err
	}
	comparison := comparePackageVersions(latest, current)
	return UpdateStatus{
		CurrentVersion:  current,
		LatestVersion:   latest,
		UpdateAvailable: comparison > 0,
		RegistryBehind:  comparison < 0,
		CheckedAt:       check.CheckedAt,
	}, nil
}

func (client *Client) joinFlight(ctx context.Context, key string, operation func(context.Context) (*Snapshot, error)) (*Snapshot, error) {
	client.mu.Lock()
	ongoing := client.flights[key]
	if ongoing == nil {
		ongoing = &flight{done: make(chan struct{})}
		client.flights[key] = ongoing
		if scope, _ := ctx.Value(operationKey{}).(*operationScope); scope != nil {
			scope.delegated.Store(true)
		}
		go client.runFlight(ctx, key, ongoing, operation)
	} else {
		var end func()
		ctx, end = trace.Start(ctx, "shared_wait")
		defer end()
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

func (client *Client) runFlight(parent context.Context, key string, ongoing *flight, operation func(context.Context) (*Snapshot, error)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), client.operationTimeout)
	defer cancel()
	snapshot, err := operation(ctx)
	client.mu.Lock()
	ongoing.snapshot, ongoing.err = snapshot, err
	delete(client.flights, key)
	close(ongoing.done)
	client.mu.Unlock()
	if scope, _ := ctx.Value(operationKey{}).(*operationScope); scope != nil {
		scope.finish(err)
	}
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
		return client.fallbackToCachedCurrent(
			ctx,
			registryBehindWarning(status.CurrentVersion, status.LatestVersion),
		)
	}
	snapshot, err := client.switchVersionOperation(ctx, status.LatestVersion)
	if err == nil {
		return snapshot, nil
	}
	var unsupported *unsupportedSchemaError
	if errors.As(err, &unsupported) {
		warning := schemaUpdateSkippedWarning(unsupported.manifest)
		snapshot, fallbackErr := client.fallbackToCachedCurrent(
			ctx,
			warning,
		)
		if fallbackErr != nil {
			return nil, fmt.Errorf("%w; compatible fallback unavailable: %v", err, fallbackErr)
		}
		return snapshot, nil
	}
	return nil, err
}

func (client *Client) fallbackToCachedCurrent(ctx context.Context, warning Warning) (*Snapshot, error) {
	snapshot, err := client.loadCachedCurrent(ctx)
	if err != nil {
		return nil, err
	}
	if warning.CurrentVersion == "" {
		warning.CurrentVersion = snapshot.Manifest.Version
	}
	if warning.Code == WarningSchemaUpdateSkipped {
		warning.Message = fmt.Sprintf(
			"ModelLink registry latest %s uses Schema v%d, while this SDK supports Schema v%d; retained active data %s",
			warning.DataPackageVersion,
			warning.DataSchemaVersion,
			SchemaInfo().SchemaVersion,
			warning.CurrentVersion,
		)
	}
	return snapshotWithWarning(snapshot, warning), nil
}

func (client *Client) pruneCache(ctx context.Context, protected ...string) error {
	pruner, ok := client.cache.(CachePruner)
	if !ok {
		return nil
	}
	ctx, end := trace.Start(ctx, "prune")
	defer end()
	return pruner.Prune(ctx, protected...)
}

func (client *Client) downloadAndStore(ctx context.Context, release artifact.Release) (*Snapshot, error) {
	pkg, err := client.resolver.Download(ctx, release)
	if err != nil {
		return nil, err
	}
	_, verifyEnd := trace.Start(ctx, "verify")
	snapshot, err := snapshotFromPackage(pkg)
	verifyEnd()
	if err != nil {
		return nil, err
	}
	writeCtx, writeEnd := trace.Start(ctx, "cache_write")
	writeErr := client.cache.Put(writeCtx, entryFromPackage(pkg))
	writeEnd()
	if err := writeErr; err != nil {
		return nil, err
	}
	client.mu.Lock()
	client.snapshots[release.Version] = snapshot
	client.mu.Unlock()
	return snapshot, nil
}

func (client *Client) acquireLock(ctx context.Context, key string) (Lock, error) {
	ctx, end := trace.Start(ctx, "lock_wait")
	defer end()
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
	if pkg.Manifest.SchemaVersion < SupportedSchemaVersion {
		return nil, &unsupportedSchemaError{manifest: pkg.Manifest}
	}
	futureSchema := pkg.Manifest.SchemaVersion > SupportedSchemaVersion
	var manifest Manifest
	if err := json.Unmarshal(pkg.Files["manifest.json"], &manifest); err != nil {
		if futureSchema {
			return nil, &unsupportedSchemaError{manifest: pkg.Manifest, cause: err}
		}
		return nil, fmt.Errorf("modellink: decode public manifest: %w", err)
	}
	var catalog Catalog
	if err := json.Unmarshal(pkg.Files["catalog.json"], &catalog); err != nil {
		if futureSchema {
			return nil, &unsupportedSchemaError{manifest: pkg.Manifest, cause: err}
		}
		return nil, fmt.Errorf("modellink: decode catalog: %w", err)
	}
	if catalog.Models == nil || catalog.Providers == nil {
		if futureSchema {
			return nil, &unsupportedSchemaError{
				manifest: pkg.Manifest,
				cause:    errors.New("core catalog maps are absent"),
			}
		}
		return nil, errors.New("modellink: catalog is missing core models or providers maps")
	}
	return &Snapshot{
		Manifest: manifest,
		Catalog:  catalog,
		files:    snapshotFiles(pkg.Files),
		warnings: schemaWarnings(manifest),
	}, nil
}

type unsupportedSchemaError struct {
	manifest artifact.Manifest
	cause    error
}

func (err *unsupportedSchemaError) Error() string {
	message := fmt.Sprintf(
		"%s: package uses Schema v%d, client requires Schema v%d",
		ErrUnsupportedSchema,
		err.manifest.SchemaVersion,
		SupportedSchemaVersion,
	)
	if err.cause != nil {
		return fmt.Sprintf("%s: %v", message, err.cause)
	}
	return message
}

func (err *unsupportedSchemaError) Unwrap() error {
	return ErrUnsupportedSchema
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

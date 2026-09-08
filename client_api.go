package modellink

import (
	"context"
	"time"

	"github.com/goroutined/modellink-go/internal/trace"
)

// Status reads persistent shared-cache metadata without querying the registry.
func (client *Client) Status(ctx context.Context) (state CacheState, err error) {
	ctx, finish := client.beginOperation(ctx, OperationStatus)
	defer func() { finish(err) }()
	ctx, end := trace.Start(ctx, "cache_read")
	defer end()
	return client.cache.State(ctx)
}

// CurrentVersion returns the active local version without accessing the
// registry. It returns ErrNoCachedData when no version has been activated.
func (client *Client) CurrentVersion(ctx context.Context) (versionResult string, err error) {
	ctx, finish := client.beginOperation(ctx, OperationCurrentVersion)
	defer func() { finish(err) }()
	return client.readCurrentVersion(ctx)
}

// FindLatest queries the registry for its latest package version. It does not
// download data or change the active version. It persists a successful check.
func (client *Client) FindLatest(ctx context.Context) (versionResult string, err error) {
	ctx, finish := client.beginOperation(ctx, OperationFindLatest)
	defer func() { finish(err) }()
	return client.findLatest(ctx)
}

// CheckLatest compares the active local version with the registry latest
// version without downloading data or activating a version. Successful checks
// are persisted in cache metadata.
func (client *Client) CheckLatest(ctx context.Context) (status UpdateStatus, err error) {
	start := time.Now()
	ctx, finish := client.beginOperation(ctx, OperationCheckLatest)
	defer func() { status.Duration = time.Since(start); finish(err) }()
	return client.checkLatest(ctx)
}

// ActivateVersion makes an already cached version active without accessing the
// registry. It returns ErrNoCachedData when that version is not cached.
func (client *Client) ActivateVersion(ctx context.Context, version string) (err error) {
	ctx, finish := client.beginOperation(ctx, OperationActivateVersion)
	defer func() { finish(err) }()
	snapshot, err := client.activateVersion(ctx, version)
	if err == nil {
		client.notifyWarnings(snapshot)
	}
	return err
}

// SwitchVersion loads and activates an explicit version. This deliberate
// operation permits both upgrades and downgrades.
func (client *Client) SwitchVersion(ctx context.Context, version string) (err error) {
	ctx, finish := client.beginOperation(ctx, OperationSwitchVersion)
	defer func() { finish(err) }()
	snapshot, err := client.switchVersion(ctx, version)
	if err == nil {
		client.notifyWarnings(snapshot)
	}
	return err
}

// LoadCached returns the active verified local snapshot without accessing the
// registry. It returns ErrNoCachedData when no version has been activated.
func (client *Client) LoadCached(ctx context.Context) (result *Snapshot, err error) {
	ctx, finish := client.beginOperation(ctx, OperationLoadCached)
	defer func() { finish(err) }()
	return client.observe(client.loadCachedCurrent(ctx))
}

// LoadVersion returns a verified explicit version without changing the active
// version. It downloads and caches the version when necessary.
func (client *Client) LoadVersion(ctx context.Context, version string) (result *Snapshot, err error) {
	ctx, finish := client.beginOperation(ctx, OperationLoadVersion)
	defer func() { finish(err) }()
	return client.observe(client.loadVersion(ctx, version))
}

// LoadLatest checks the registry, prevents automatic downgrade, activates the
// safe latest version and returns the active verified snapshot.
func (client *Client) LoadLatest(ctx context.Context) (result *Snapshot, err error) {
	ctx, finish := client.beginOperation(ctx, OperationLoadLatest)
	defer func() { finish(err) }()
	return client.observe(client.loadLatest(ctx))
}

// Load returns the active verified cache without checking the registry. When
// no valid cache is available, it blocks while loading the safe latest version.
func (client *Client) Load(ctx context.Context) (result *Snapshot, err error) {
	ctx, finish := client.beginOperation(ctx, OperationLoad)
	defer func() { finish(err) }()
	return client.observe(client.load(ctx))
}

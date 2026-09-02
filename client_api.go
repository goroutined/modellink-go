package modellink

import "context"

// CurrentVersion returns the active local version without accessing the
// registry. It returns ErrNoCachedData when no version has been activated.
func (client *Client) CurrentVersion(ctx context.Context) (string, error) {
	return client.readCurrentVersion(ctx)
}

// FindLatest queries the registry for its latest package version. It does not
// download data, write the cache or change the active version.
func (client *Client) FindLatest(ctx context.Context) (string, error) {
	return client.findLatest(ctx)
}

// CheckLatest compares the active local version with the registry latest
// version without downloading data or changing the cache.
func (client *Client) CheckLatest(ctx context.Context) (UpdateStatus, error) {
	return client.checkLatest(ctx)
}

// ActivateVersion makes an already cached version active without accessing the
// registry. It returns ErrNoCachedData when that version is not cached.
func (client *Client) ActivateVersion(ctx context.Context, version string) error {
	snapshot, err := client.activateVersion(ctx, version)
	if err == nil {
		client.notifyWarnings(snapshot)
	}
	return err
}

// SwitchVersion loads and activates an explicit version. This deliberate
// operation permits both upgrades and downgrades.
func (client *Client) SwitchVersion(ctx context.Context, version string) error {
	snapshot, err := client.switchVersion(ctx, version)
	if err == nil {
		client.notifyWarnings(snapshot)
	}
	return err
}

// LoadCached returns the active verified local snapshot without accessing the
// registry. It returns ErrNoCachedData when no version has been activated.
func (client *Client) LoadCached(ctx context.Context) (*Snapshot, error) {
	return client.observe(client.loadCachedCurrent(ctx))
}

// LoadVersion returns a verified explicit version without changing the active
// version. It downloads and caches the version when necessary.
func (client *Client) LoadVersion(ctx context.Context, version string) (*Snapshot, error) {
	return client.observe(client.loadVersion(ctx, version))
}

// LoadLatest checks the registry, prevents automatic downgrade, activates the
// safe latest version and returns the active verified snapshot.
func (client *Client) LoadLatest(ctx context.Context) (*Snapshot, error) {
	return client.observe(client.loadLatest(ctx))
}

// Load returns the active verified cache without checking the registry. When
// no valid cache is available, it blocks while loading the safe latest version.
func (client *Client) Load(ctx context.Context) (*Snapshot, error) {
	return client.observe(client.load(ctx))
}

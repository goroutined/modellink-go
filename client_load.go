package modellink

import (
	"context"
	"fmt"

	"github.com/goroutined/modellink-go/internal/artifact"
)

// Load returns the active verified snapshot without checking the network. If
// no active snapshot exists yet, it downloads and activates the latest release.
func (client *Client) Load(ctx context.Context) (*Snapshot, error) {
	client.mu.Lock()
	current := client.current
	client.mu.Unlock()
	if current != nil {
		return client.observe(current, nil)
	}
	version, err := client.cache.Current(ctx)
	if err == nil {
		snapshot, loadErr := client.loadCachedWithLock(ctx, version)
		if loadErr == nil {
			client.setCurrent(snapshot)
			return client.observe(snapshot, nil)
		}
	}
	return client.LoadLatest(ctx)
}

// LoadVersion returns a verified package version without changing the active
// version used by Load.
func (client *Client) LoadVersion(ctx context.Context, version string) (*Snapshot, error) {
	if !versionPattern.MatchString(version) {
		return nil, fmt.Errorf("modellink: invalid package version %q", version)
	}
	snapshot, err := client.joinFlight(ctx, "version:"+version, func(ctx context.Context) (*Snapshot, error) {
		maintenance, err := client.acquireLock(ctx, "maintenance")
		if err != nil {
			return nil, err
		}
		snapshot, err := client.loadExactVersion(ctx, version)
		if err != nil {
			_ = maintenance.Unlock()
			return nil, err
		}
		if err := client.pruneCache(ctx, version); err != nil {
			_ = maintenance.Unlock()
			return nil, err
		}
		if err := maintenance.Unlock(); err != nil {
			return nil, err
		}
		return snapshot, nil
	})
	return client.observe(snapshot, err)
}

func (client *Client) loadExactVersion(ctx context.Context, version string) (*Snapshot, error) {
	lock, err := client.acquireLock(ctx, "version:"+version)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	if snapshot, err := client.loadCached(ctx, version); err == nil {
		return snapshot, nil
	}
	release, err := client.resolver.Resolve(ctx, version)
	if err != nil {
		return nil, err
	}
	if release.Version != version {
		return nil, fmt.Errorf("modellink: registry resolved %q as %q", version, release.Version)
	}
	return client.downloadAndStore(ctx, release)
}

func (client *Client) loadResolvedVersion(ctx context.Context, release artifact.Release) (*Snapshot, error) {
	lock, err := client.acquireLock(ctx, "version:"+release.Version)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	if snapshot, err := client.loadCached(ctx, release.Version); err == nil {
		return snapshot, nil
	}
	return client.downloadAndStore(ctx, release)
}

func (client *Client) loadCachedWithLock(ctx context.Context, version string) (*Snapshot, error) {
	lock, err := client.acquireLock(ctx, "version:"+version)
	if err != nil {
		return nil, err
	}
	snapshot, loadErr := client.loadCached(ctx, version)
	unlockErr := lock.Unlock()
	if loadErr != nil {
		return nil, loadErr
	}
	if unlockErr != nil {
		return nil, unlockErr
	}
	return snapshot, nil
}

func (client *Client) loadCached(ctx context.Context, version string) (*Snapshot, error) {
	client.mu.Lock()
	snapshot := client.snapshots[version]
	client.mu.Unlock()
	if snapshot != nil {
		return snapshot, nil
	}
	entry, err := client.cache.Get(ctx, version)
	if err != nil {
		return nil, err
	}
	pkg, err := packageFromEntry(entry)
	if err != nil {
		return nil, err
	}
	snapshot, err = snapshotFromPackage(pkg)
	if err != nil {
		return nil, err
	}
	client.mu.Lock()
	if existing := client.snapshots[version]; existing != nil {
		snapshot = existing
	} else {
		client.snapshots[version] = snapshot
	}
	client.mu.Unlock()
	return snapshot, nil
}

func (client *Client) currentVersion() string {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.current == nil {
		return ""
	}
	return client.current.Manifest.Version
}

func (client *Client) setCurrent(snapshot *Snapshot) {
	client.mu.Lock()
	client.current = snapshot
	client.mu.Unlock()
}

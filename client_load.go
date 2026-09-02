package modellink

import (
	"context"
	"errors"
	"fmt"
)

func (client *Client) load(ctx context.Context) (*Snapshot, error) {
	snapshot, err := client.loadCachedCurrent(ctx)
	if err == nil {
		return snapshot, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return client.loadLatest(ctx)
}

func (client *Client) loadCachedCurrent(ctx context.Context) (*Snapshot, error) {
	version, err := client.cache.Current(ctx)
	if err == nil {
		snapshot, loadErr := client.loadCachedWithLock(ctx, version)
		if loadErr != nil {
			return nil, loadErr
		}
		client.setCurrent(snapshot)
		return snapshot, nil
	}
	if !errors.Is(err, ErrNoCachedData) {
		return nil, err
	}
	client.mu.Lock()
	current := client.current
	client.mu.Unlock()
	if current != nil {
		return current, nil
	}
	return nil, ErrNoCachedData
}

func (client *Client) loadVersion(ctx context.Context, version string) (*Snapshot, error) {
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	return client.joinFlight(ctx, "version:"+version, func(ctx context.Context) (*Snapshot, error) {
		return client.prepareVersion(ctx, version)
	})
}

func (client *Client) prepareVersion(ctx context.Context, version string) (*Snapshot, error) {
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
}

func (client *Client) activateVersion(ctx context.Context, version string) (*Snapshot, error) {
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	return client.joinFlight(ctx, "activate:"+version, func(ctx context.Context) (*Snapshot, error) {
		maintenance, err := client.acquireLock(ctx, "maintenance")
		if err != nil {
			return nil, err
		}
		snapshot, err := client.activateCachedVersion(ctx, version)
		unlockErr := maintenance.Unlock()
		if err != nil {
			return nil, err
		}
		if unlockErr != nil {
			return nil, unlockErr
		}
		return snapshot, nil
	})
}

func (client *Client) switchVersion(ctx context.Context, version string) (*Snapshot, error) {
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	return client.joinFlight(ctx, "switch:"+version, func(ctx context.Context) (*Snapshot, error) {
		return client.switchVersionOperation(ctx, version)
	})
}

func (client *Client) switchVersionOperation(ctx context.Context, version string) (*Snapshot, error) {
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
	snapshot, err = client.activateCachedVersion(ctx, version)
	unlockErr := maintenance.Unlock()
	if err != nil {
		return nil, err
	}
	if unlockErr != nil {
		return nil, unlockErr
	}
	return snapshot, nil
}

func (client *Client) activateCachedVersion(ctx context.Context, version string) (*Snapshot, error) {
	lock, err := client.acquireLock(ctx, "version:"+version)
	if err != nil {
		return nil, err
	}
	snapshot, loadErr := client.loadCached(ctx, version)
	if loadErr == nil {
		loadErr = client.cache.SetCurrent(ctx, version)
	}
	unlockErr := lock.Unlock()
	if loadErr != nil {
		return nil, loadErr
	}
	if unlockErr != nil {
		return nil, unlockErr
	}
	client.setCurrent(snapshot)
	return snapshot, nil
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

func (client *Client) readCurrentVersion(ctx context.Context) (string, error) {
	version, err := client.cache.Current(ctx)
	if err == nil {
		return version, nil
	}
	if !errors.Is(err, ErrNoCachedData) {
		return "", err
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.current != nil {
		return client.current.Manifest.Version, nil
	}
	return "", ErrNoCachedData
}

func (client *Client) setCurrent(snapshot *Snapshot) {
	client.mu.Lock()
	client.current = snapshot
	client.mu.Unlock()
}

func validateVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("modellink: invalid package version %q", version)
	}
	return nil
}

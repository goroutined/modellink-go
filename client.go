package modellink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/goroutined/modellink-go/internal/artifact"
)

var (
	ErrNoCachedData      = errors.New("modellink: no cached data")
	ErrUnsupportedSchema = errors.New("modellink: unsupported schema version")
	ErrLockTimeout       = errors.New("modellink: cache lock timeout")
)

var versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

type Options struct {
	// Registry is an npm-compatible registry URL. Empty uses npmmirror.
	Registry string
	// Cache stores and coordinates packages. Empty uses the default FileCache.
	Cache Cache
}

type Client struct {
	resolver         artifact.Resolver
	cache            Cache
	operationTimeout time.Duration
	lockTimeout      time.Duration

	mu        sync.Mutex
	current   *Snapshot
	snapshots map[string]*Snapshot
	flights   map[string]*flight
}

type Release struct {
	Version   string
	Tarball   string
	Integrity string
}

type UpdateStatus struct {
	CurrentVersion  string
	LatestVersion   string
	UpdateAvailable bool
}

type Snapshot struct {
	Manifest Manifest
	Catalog  Catalog

	files map[DataFile][]byte
}

func (snapshot *Snapshot) Schema() []byte {
	contents, _ := snapshot.File(FileSchema)
	return contents
}

type DataFile string

const (
	FileAPI      DataFile = "api.json"
	FileModels   DataFile = "models.json"
	FileCatalog  DataFile = "catalog.json"
	FileSchema   DataFile = "schema.json"
	FileManifest DataFile = "manifest.json"
)

func (snapshot *Snapshot) File(name DataFile) ([]byte, bool) {
	if snapshot == nil {
		return nil, false
	}
	contents, ok := snapshot.files[name]
	return append([]byte(nil), contents...), ok
}

type flight struct {
	done     chan struct{}
	snapshot *Snapshot
	err      error
	waiters  int
}

func New(options Options) (*Client, error) {
	cache := options.Cache
	if cache == nil {
		var err error
		cache, err = NewFileCache(FileCacheOptions{})
		if err != nil {
			return nil, err
		}
	}
	const operationTimeout = 3 * time.Minute
	const lockTimeout = 2 * time.Minute
	return &Client{
		resolver: artifact.Resolver{Registry: options.Registry, HTTPClient: &http.Client{Timeout: time.Minute}},
		cache:    cache, operationTimeout: operationTimeout, lockTimeout: lockTimeout,
		snapshots: make(map[string]*Snapshot), flights: make(map[string]*flight),
	}, nil
}

func (client *Client) Latest(ctx context.Context) (Release, error) {
	release, err := client.resolver.Resolve(ctx, "latest")
	if err != nil {
		return Release{}, err
	}
	return publicRelease(release), nil
}

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

// Load returns the active verified snapshot without checking the network. If
// no active snapshot exists yet, it downloads and activates the latest release.
func (client *Client) Load(ctx context.Context) (*Snapshot, error) {
	client.mu.Lock()
	current := client.current
	client.mu.Unlock()
	if current != nil {
		return current, nil
	}
	version, err := client.cache.Current(ctx)
	if err == nil {
		snapshot, loadErr := client.loadCachedWithLock(ctx, version)
		if loadErr == nil {
			client.setCurrent(snapshot)
			return snapshot, nil
		}
	}
	return client.LoadLatest(ctx)
}

// LoadLatest checks the configured registry, downloads the latest release when
// necessary, and atomically makes it the active snapshot.
func (client *Client) LoadLatest(ctx context.Context) (*Snapshot, error) {
	return client.joinFlight(ctx, "latest", client.updateLatest)
}

// LoadVersion returns a verified package version without changing the active
// version used by Load.
func (client *Client) LoadVersion(ctx context.Context, version string) (*Snapshot, error) {
	if !versionPattern.MatchString(version) {
		return nil, fmt.Errorf("modellink: invalid package version %q", version)
	}
	return client.joinFlight(ctx, "version:"+version, func(ctx context.Context) (*Snapshot, error) {
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

	// Resolve only after taking the global latest lock. An older resolver result
	// can therefore never activate after a newer process has already updated.
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
	// Another process may have installed the package while this client waited.
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
	return &Snapshot{Manifest: manifest, Catalog: catalog, files: snapshotFiles(pkg.Files)}, nil
}

func publicRelease(release artifact.Release) Release {
	return Release{
		Version:   release.Version,
		Tarball:   release.Tarball,
		Integrity: release.Integrity,
	}
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

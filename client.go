package modellink

import (
	"errors"
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

// Options configures a Client. Its zero value uses npmmirror and the default
// cross-platform file cache.
type Options struct {
	// Registry is an npm-compatible registry URL. Empty uses npmmirror.
	Registry string
	// Cache stores and coordinates packages. Empty uses the default FileCache.
	Cache Cache
	// OnWarning receives each distinct non-fatal warning once per Client. Nil
	// keeps the client silent; warnings remain available from Snapshot.Warnings.
	OnWarning func(Warning)
}

// Client loads verified, versioned ModelLink snapshots.
type Client struct {
	resolver         artifact.Resolver
	cache            Cache
	operationTimeout time.Duration
	lockTimeout      time.Duration

	mu        sync.Mutex
	current   *Snapshot
	snapshots map[string]*Snapshot
	flights   map[string]*flight
	onWarning func(Warning)
	warned    map[string]struct{}
}

// Release describes one published @modellink/data package.
type Release struct {
	Version   string
	Tarball   string
	Integrity string
}

// UpdateStatus compares the active snapshot with the registry latest version.
type UpdateStatus struct {
	CurrentVersion  string
	LatestVersion   string
	UpdateAvailable bool
}

type flight struct {
	done     chan struct{}
	snapshot *Snapshot
	err      error
	waiters  int
}

// New creates a ModelLink client.
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
		resolver: artifact.Resolver{
			Registry:   options.Registry,
			HTTPClient: &http.Client{Timeout: time.Minute},
		},
		cache:            cache,
		operationTimeout: operationTimeout,
		lockTimeout:      lockTimeout,
		snapshots:        make(map[string]*Snapshot),
		flights:          make(map[string]*flight),
		onWarning:        options.OnWarning,
		warned:           make(map[string]struct{}),
	}, nil
}

func (client *Client) observe(snapshot *Snapshot, err error) (*Snapshot, error) {
	if err == nil && snapshot != nil {
		client.notifyWarnings(snapshot)
	}
	return snapshot, err
}

func (client *Client) notifyWarnings(snapshot *Snapshot) {
	if client.onWarning == nil {
		return
	}
	for _, warning := range snapshot.warnings {
		key := string(warning.Code) + "\x00" + warning.DataPackageVersion + "\x00" + warning.DataSchemaSHA256
		client.mu.Lock()
		_, seen := client.warned[key]
		if !seen {
			client.warned[key] = struct{}{}
		}
		client.mu.Unlock()
		if !seen {
			client.onWarning(warning)
		}
	}
}

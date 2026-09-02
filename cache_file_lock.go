package modellink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

func (cache *FileCache) Lock(ctx context.Context, key string) (Lock, error) {
	if key == "" {
		return nil, errors.New("modellink: cache lock key is empty")
	}
	locks := filepath.Join(cache.directory, ".locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		return nil, fmt.Errorf("modellink: create lock directory: %w", err)
	}
	digest := sha256.Sum256([]byte(key))
	path := filepath.Join(locks, hex.EncodeToString(digest[:])+".lock")
	fileLock := flock.New(path)
	locked, err := fileLock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("modellink: acquire cache lock %q: %w", key, err)
	}
	if !locked {
		return nil, fmt.Errorf("modellink: acquire cache lock %q: %w", key, ctx.Err())
	}
	return fileLock, nil
}

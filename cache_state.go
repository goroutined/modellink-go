package modellink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goroutined/modellink-go/internal/atomicfile"
	"github.com/goroutined/modellink-go/internal/trace"
)

// State reads an atomic metadata snapshot, including old version-only files.
func (cache *FileCache) State(ctx context.Context) (CacheState, error) {
	if err := ctx.Err(); err != nil {
		return CacheState{}, err
	}
	contents, err := os.ReadFile(filepath.Join(cache.directory, "current.json"))
	if errors.Is(err, os.ErrNotExist) {
		return CacheState{}, nil
	}
	if err != nil {
		return CacheState{}, err
	}
	var state CacheState
	if err := json.Unmarshal(contents, &state); err != nil {
		return CacheState{}, fmt.Errorf("modellink: decode cache state: %w", err)
	}
	if state.Version != "" && !versionPattern.MatchString(state.Version) {
		return CacheState{}, errors.New("modellink: invalid current version")
	}
	return state, nil
}

func (cache *FileCache) RecordCheck(ctx context.Context, check CheckState) error {
	if err := validateVersion(check.LatestVersion); err != nil {
		return err
	}
	if check.CheckedAt.IsZero() || check.Registry == "" {
		return errors.New("modellink: incomplete check state")
	}
	return cache.changeState(ctx, func(state *CacheState) {
		if state.LastCheck == nil || check.CheckedAt.After(state.LastCheck.CheckedAt) {
			check.CheckedAt = check.CheckedAt.UTC()
			state.LastCheck = &check
		}
	})
}

// Metadata has its own lock: callers may already hold maintenance/version locks.
// Never acquire a version lock while holding this metadata lock.
func (cache *FileCache) changeState(ctx context.Context, change func(*CacheState)) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	lockCtx, lockEnd := trace.Start(ctx, "lock_wait")
	lock, err := cache.Lock(lockCtx, "metadata")
	lockEnd()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	state, err := cache.State(ctx)
	if err != nil {
		return err
	}
	change(&state)
	contents, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(cache.directory, "current.json"), append(contents, '\n'), 0o600)
}

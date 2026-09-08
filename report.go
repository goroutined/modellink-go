package modellink

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goroutined/modellink-go/internal/trace"
)

// Operation identifies the public entry point that initiated a logical task.
type Operation string

const (
	OperationCurrentVersion  Operation = "current_version"
	OperationStatus          Operation = "status"
	OperationFindLatest      Operation = "find_latest"
	OperationCheckLatest     Operation = "check_latest"
	OperationActivateVersion Operation = "activate_version"
	OperationSwitchVersion   Operation = "switch_version"
	OperationLoadCached      Operation = "load_cached"
	OperationLoadVersion     Operation = "load_version"
	OperationLoadLatest      Operation = "load_latest"
	OperationLoad            Operation = "load"
)

// Stage identifies one measured phase of an operation. A phase may occur more
// than once; an absent phase was not executed by this operation.
type Stage string

const (
	StageLockWait   Stage = "lock_wait"
	StageResolve    Stage = "resolve"
	StageCacheRead  Stage = "cache_read"
	StageDownload   Stage = "download"
	StageVerify     Stage = "verify"
	StageCacheWrite Stage = "cache_write"
	StageActivate   Stage = "activate"
	StagePrune      Stage = "prune"
	StageSharedWait Stage = "shared_wait"
)

// StageReport measures one phase, excluding time attributed to nested phases.
// Reports are ordered by phase start time, not aggregated by Stage.
type StageReport struct {
	Stage    Stage
	Duration time.Duration
}

// OperationReport measures a logical operation. A joined caller reports only
// its own shared_wait; the original task reports its work even if its caller
// cancels. Err is the operation error; applications should apply their usual
// redaction policy before exporting error messages to external log systems.
// Reports are not persisted. Callbacks run outside client/cache locks, may run
// concurrently, and may arrive after the initiating API call has returned.
type OperationReport struct {
	Operation Operation
	// StartedAt and FinishedAt are UTC wall-clock timestamps.
	StartedAt  time.Time
	FinishedAt time.Time
	// Duration includes unclassified overhead but excludes the report callback.
	// It need not equal the sum of the exclusive stage durations.
	Duration time.Duration
	Stages   []StageReport
	Err      error
}
type operationKey struct{}
type operationScope struct {
	operation Operation
	start     time.Time
	recorder  *trace.Recorder
	callback  func(OperationReport)
	delegated atomic.Bool
	once      sync.Once
}

func (client *Client) beginOperation(ctx context.Context, op Operation) (context.Context, func(error)) {
	if client.onOperation == nil {
		return ctx, func(error) {}
	}
	ctx, recorder := trace.New(ctx)
	scope := &operationScope{operation: op, start: time.Now(), recorder: recorder, callback: client.onOperation}
	ctx = context.WithValue(ctx, operationKey{}, scope)
	return ctx, func(err error) {
		if !scope.delegated.Load() {
			scope.finish(err)
		}
	}
}
func (scope *operationScope) finish(err error) {
	if scope == nil {
		return
	}
	scope.once.Do(func() {
		end := time.Now()
		report := OperationReport{Operation: scope.operation, StartedAt: scope.start.UTC(), FinishedAt: end.UTC(), Duration: end.Sub(scope.start), Err: err}
		for _, stage := range scope.recorder.Stages() {
			report.Stages = append(report.Stages, StageReport{Stage: Stage(stage.Name), Duration: stage.Duration})
		}
		scope.callback(report)
	})
}

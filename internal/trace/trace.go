// Package trace measures exclusive stage durations without exposing SDK types
// to the artifact transport. Contexts carry a recorder only when requested.
package trace

import (
	"context"
	"sync"
	"time"
)

type Stage struct {
	Name     string
	Duration time.Duration
}
type Recorder struct {
	mu     sync.Mutex
	stages []Stage
}
type key struct{}
type frameKey struct{}
type frame struct{ children time.Duration }

func New(ctx context.Context) (context.Context, *Recorder) {
	r := &Recorder{}
	return context.WithValue(ctx, key{}, r), r
}

// Start records stages in start order. Nested time is excluded from parents.
// A stage context must not be used concurrently by independent tasks.
func Start(ctx context.Context, name string) (context.Context, func()) {
	r, _ := ctx.Value(key{}).(*Recorder)
	if r == nil {
		return ctx, func() {}
	}
	parent, _ := ctx.Value(frameKey{}).(*frame)
	f := &frame{}
	start := time.Now()
	r.mu.Lock()
	i := len(r.stages)
	r.stages = append(r.stages, Stage{Name: name})
	r.mu.Unlock()
	return context.WithValue(ctx, frameKey{}, f), func() {
		elapsed := time.Since(start)
		r.mu.Lock()
		r.stages[i].Duration = elapsed - f.children
		if parent != nil {
			parent.children += elapsed
		}
		r.mu.Unlock()
	}
}

func (r *Recorder) Stages() []Stage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Stage(nil), r.stages...)
}

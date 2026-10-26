package sync

import "context"

// ProgressReport receives a sync pass's progress: in phase, done of total
// files handled so far. It is called from worker goroutines, so it must be
// safe for concurrent use, and it should return quickly.
type ProgressReport func(phase string, done, total int)

// Sync pass phases.
const (
	PhaseCompare  = "compare"  // comparing each file with storage (hashing what changed)
	PhaseTransfer = "transfer" // uploading, downloading and merging
)

type progressKey struct{}

// WithProgress returns a context that makes Sync report its progress to fn
// (the agent passes it on to the menu bar and the Stradl app).
func WithProgress(ctx context.Context, fn ProgressReport) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func progressFrom(ctx context.Context) ProgressReport {
	if fn, ok := ctx.Value(progressKey{}).(ProgressReport); ok && fn != nil {
		return fn
	}
	return func(string, int, int) {}
}

package sync

import "context"

// ProgressReport receives a sync pass's progress: done of total files
// handled so far. It is called from worker goroutines, so it must be safe
// for concurrent use, and it should return quickly.
type ProgressReport func(done, total int)

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
	return func(int, int) {}
}

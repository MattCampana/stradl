package dashboard

import (
	"context"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

// LocalOptions configure the real Provider.
type LocalOptions struct {
	// LoadConfig returns the current Stradl config (re-read on each request so
	// a re-init is picked up).
	LoadConfig func() (*config.Config, error)
	// StatusPath is the agent's status.json.
	StatusPath string
	// AgentRunning reports whether the background agent is running.
	AgentRunning func() bool
	// SyncNow triggers an immediate sync; nil when no agent is attached.
	SyncNow func() error
	// Now returns the current time (tests).
	Now func() time.Time
}

// NewLocalProvider returns the Provider backed by this Mac's data.
//
// STUB: replaced by the real implementation (local.go). Kept so the server and
// the agent integration compile while it is built.
func NewLocalProvider(opts LocalOptions) Provider { return &stubProvider{opts: opts} }

type stubProvider struct{ opts LocalOptions }

func (p *stubProvider) Overview(context.Context) (*Overview, error) {
	return &Overview{GeneratedAt: time.Now()}, nil
}

func (p *stubProvider) Session(context.Context, string) (*SessionDetail, error) {
	return nil, ErrNotFound
}

func (p *stubProvider) SyncNow() error {
	if p.opts.SyncNow == nil {
		return nil
	}
	return p.opts.SyncNow()
}

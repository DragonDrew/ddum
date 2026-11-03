// Package scheduler runs the bot's periodic jobs: registry checks,
// offline detection, and retention.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/store"
)

// OfflineStore marks stale hosts offline.
type OfflineStore interface {
	MarkOfflineHosts(ctx context.Context, now time.Time) ([]store.Host, error)
}

// OfflineNotifier posts the offline notice.
type OfflineNotifier interface {
	HostOffline(ctx context.Context, h store.Host)
}

// OfflineSweeper marks hosts offline when their agent stops polling and posts
// one notice per transition. State lives in the database, so
// a bot restart simply resumes the sweep.
type OfflineSweeper struct {
	Store  OfflineStore
	Notify OfflineNotifier
	Log    *slog.Logger
	Now    func() time.Time
	Every  time.Duration
}

// Run sweeps every Every until ctx is cancelled.
func (o *OfflineSweeper) Run(ctx context.Context) {
	t := time.NewTicker(o.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.Sweep(ctx)
		}
	}
}

// Sweep runs once.
func (o *OfflineSweeper) Sweep(ctx context.Context) {
	hosts, err := o.Store.MarkOfflineHosts(ctx, o.Now())
	if err != nil {
		if ctx.Err() == nil {
			o.Log.Error("offline sweep", "err", err)
		}
		return
	}
	for _, h := range hosts {
		o.Log.Warn("host offline", "host_id", h.ID, "agent_id", h.AgentID, "last_seen_at", h.LastSeenAt)
		if o.Notify != nil {
			o.Notify.HostOffline(ctx, h)
		}
	}
}

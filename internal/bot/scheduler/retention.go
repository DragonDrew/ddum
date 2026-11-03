package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/store"
)

// Retention deletes operation events older than 180 days once a day
// .
type Retention struct {
	Store *store.Store
	Log   *slog.Logger
	Now   func() time.Time
}

// Run prunes shortly after startup and then daily until ctx ends.
func (r *Retention) Run(ctx context.Context) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 24 * time.Hour
		n, err := r.Store.PruneOperationEvents(ctx, r.Now().Add(-store.OperationEventRetention))
		switch {
		case err != nil && ctx.Err() == nil:
			r.Log.Error("operation event retention", "err", err)
		case n > 0:
			r.Log.Info("pruned old operation events", "count", n)
		}
	}
}

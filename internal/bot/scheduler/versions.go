package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
)

// VersionStore finds agents whose version differs from the bot's.
type VersionStore interface {
	TakeAgentVersionNotices(ctx context.Context, botVersion string, seenSince time.Time) ([]store.Host, error)
}

// VersionNotifier posts the version notice.
type VersionNotifier interface {
	AgentVersionDiffers(ctx context.Context, h store.Host, botVersion string)
}

// AgentVersions posts one notice per host whose agent runs a version other
// than the bot's. Agents are protected and updated by hand,
// so one is easily left behind after a release. Each agent and bot version
// pair is reported once, and only for agents that have polled since Started.
type AgentVersions struct {
	Store   VersionStore
	Notify  VersionNotifier
	Log     *slog.Logger
	Version string    // the bot's version
	Started time.Time // when the bot started
	Every   time.Duration
}

// Run sweeps every Every until ctx is cancelled. A development build of the
// bot has no version to compare, so it does nothing.
func (a *AgentVersions) Run(ctx context.Context) {
	if !releaseVersion(a.Version) {
		return
	}
	t := time.NewTicker(a.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Sweep(ctx)
		}
	}
}

// Sweep runs once.
func (a *AgentVersions) Sweep(ctx context.Context) {
	hosts, err := a.Store.TakeAgentVersionNotices(ctx, a.Version, a.Started)
	if err != nil {
		if ctx.Err() == nil {
			a.Log.Error("agent version sweep", "err", err)
		}
		return
	}
	for _, h := range hosts {
		if !releaseVersion(h.AgentVersion) {
			continue // a development agent is run on purpose
		}
		a.Log.Info("agent version differs", "host_id", h.ID, "agent_version", h.AgentVersion, "bot_version", a.Version)
		if a.Notify != nil {
			a.Notify.AgentVersionDiffers(ctx, h, a.Version)
		}
	}
}

func releaseVersion(v string) bool {
	_, ok := domain.ParseVersionTag(v)
	return ok
}

//go:build rig

package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/discord"
)

// Only rig builds can run DISCORD_MODE=fake: its test endpoints let anyone
// holding the shared key act as any Discord user.
func init() {
	fakeDiscord = func(log *slog.Logger, key []byte) (discord.Gateway, func(*discord.Router, http.Handler) http.Handler) {
		gw := &discord.LoggingFake{Log: log.With("subsystem", "fake-discord")}
		gw.Prefix = fmt.Sprintf("boot%d-", time.Now().Unix())
		return gw, func(router *discord.Router, api http.Handler) http.Handler {
			mux := http.NewServeMux()
			mux.Handle("/test/v1/", discord.TestHandler(router, gw, key, time.Now))
			mux.Handle("/", api)
			return mux
		}
	}
}

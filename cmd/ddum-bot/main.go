// Command ddum-bot is the central Discord bot and coordination service.
//
// Usage:
//
//	ddum-bot              run the bot (same as "ddum-bot run")
//	ddum-bot healthcheck  exit 0 if the local bot answers /healthz
//	ddum-bot backup PATH  write a consistent copy of the database to PATH
//	ddum-bot version      print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/config"
	"github.com/DragonDrew/ddum/internal/bot/discord"
	"github.com/DragonDrew/ddum/internal/bot/httpapi"
	"github.com/DragonDrew/ddum/internal/bot/orchestrator"
	"github.com/DragonDrew/ddum/internal/bot/registry"
	"github.com/DragonDrew/ddum/internal/bot/scheduler"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/buildinfo"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/logging"
)

// fakeDiscord is set in rig builds only (fake_rig.go).
var fakeDiscord func(log *slog.Logger, key []byte) (discord.Gateway, func(*discord.Router, http.Handler) http.Handler)

// Exit codes.
const (
	exitOK     = 0
	exitError  = 1
	exitConfig = 2
)

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "run":
		os.Exit(run())
	case "healthcheck":
		os.Exit(healthcheck())
	case "backup":
		os.Exit(backup(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println(buildinfo.Version)
	case "help", "--help", "-h":
		fmt.Println("usage: ddum-bot [run|healthcheck|backup <path>|version]")
	default:
		fmt.Fprintf(os.Stderr, "ddum-bot: unknown command %q (want run, healthcheck, backup, or version)\n", cmd)
		os.Exit(exitConfig)
	}
}

func run() int {
	started := time.Now()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ddum-bot: invalid configuration:\n%v\n", err)
		return exitConfig
	}
	log := logging.New(os.Stdout, cfg.Level, "bot")
	log.Info("starting", "version", buildinfo.Version, "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabasePath, ids.V7{})
	if err != nil {
		log.Error("open database", "path", cfg.DatabasePath, "err", err)
		return exitError
	}
	defer db.Close()
	version, _ := db.SchemaVersion(ctx)
	log.Info("database ready", "path", cfg.DatabasePath, "schema_version", version)

	// Discord: the session is the gateway for notices and the source of
	// interactions. DISCORD_MODE=fake (rig builds only) keeps messages in
	// memory and takes interactions from signed /test/v1 requests.
	var gateway discord.Gateway
	var session *discord.Session
	var addTestAPI func(*discord.Router, http.Handler) http.Handler
	if cfg.DiscordMode == "fake" {
		if fakeDiscord == nil {
			log.Error("DISCORD_MODE=fake only works in a rig build (go build -tags rig)")
			return exitConfig
		}
		log.Warn("DISCORD_MODE=fake: test mode with no Discord connection; /test/v1 endpoints are enabled")
		gateway, addTestAPI = fakeDiscord(log, []byte(cfg.AgentSharedKey))
	} else {
		session, err = discord.NewSession(cfg.DiscordToken, cfg.DiscordApplicationID, cfg.DiscordGuildID, log.With("subsystem", "discord"))
		if err != nil {
			log.Error("discord", "err", err)
			return exitError
		}
		gateway = session
	}
	notifier := discord.NewNotifier(discord.Notifier{
		Gateway: gateway, Store: db, ChannelID: cfg.NotificationChannelID,
		Log: log.With("subsystem", "notify"), Now: time.Now, Limit: cfg.NotifyIndividualLimit,
	}, 256)
	notifyCtx, stopNotify := context.WithCancel(context.Background())
	notifyDone := make(chan struct{})
	go func() { notifier.Run(notifyCtx); close(notifyDone) }()
	defer func() { stopNotify(); <-notifyDone }()

	checker := &scheduler.Checker{
		Store: db,
		Registry: registry.New(registry.Config{ConcurrencyPerHost: cfg.RegistryConcurrencyPerHost,
			GHCRToken: cfg.GHCRToken, UserAgent: "ddum-bot/" + buildinfo.Version}),
		Notify: notifier, Log: log.With("subsystem", "registry"), Now: time.Now,
		Interval: cfg.RegistryCheckInterval, GraceHours: cfg.StackMissingGraceHours,
		LinkCheck: func(ctx context.Context, url string) bool {
			return registry.LinkExists(ctx, http.DefaultClient, url)
		},
	}
	auto := &orchestrator.AutoDeployer{Store: db, Notify: notifier, Log: log.With("subsystem", "auto-update"), Now: time.Now,
		ExpiryMinutes: cfg.CommandExpiryMinutes}
	checker.AfterCheck = auto.Trigger
	startCheck := func(hostID, stackID string, done func(discord.CheckSummary)) error {
		finished := func(res scheduler.CheckResult, err error) {
			done(discord.CheckSummary{Checked: res.Checked, NewUpdates: res.NewUpdates, Errors: res.Errors, Err: err})
		}
		if err := checker.Start(scheduler.Scope{HostID: hostID, StackID: stackID}, finished); errors.Is(err, scheduler.ErrBusy) {
			return discord.ErrCheckBusy
		} else if err != nil {
			return err
		}
		return nil
	}

	router := discord.NewRouter(discord.Router{
		Store: db, Gateway: gateway, Log: log.With("subsystem", "discord"), Now: time.Now, Started: started,
		Version: buildinfo.Version, GuildID: cfg.DiscordGuildID, ChannelID: cfg.NotificationChannelID,
		Notify: notifier, StartCheck: startCheck, ExpiryMinutes: cfg.CommandExpiryMinutes, Location: cfg.Location,
	}, cfg.AuthorizedDiscordIDs)
	if session != nil {
		if err := session.Start(router); err != nil {
			log.Error("discord", "err", err)
			return exitError
		}
		defer session.Close()
		checkChannel(ctx, session, notifier, cfg.NotificationChannelID, log)
	}
	go checker.Run(ctx)
	go auto.Run(ctx)
	opSweeper := &orchestrator.Sweeper{Store: db, Notify: notifier, Log: log.With("subsystem", "operations"), Now: time.Now,
		Every: 15 * time.Second}
	go opSweeper.Run(ctx)
	go (&scheduler.Retention{Store: db, Log: log.With("subsystem", "retention"), Now: time.Now}).Run(ctx)

	sweeper := &scheduler.OfflineSweeper{Store: db, Notify: notifier, Log: log.With("subsystem", "scheduler"),
		Now: time.Now, Every: 15 * time.Second}
	go sweeper.Run(ctx)
	go (&scheduler.AgentVersions{Store: db, Notify: notifier, Log: log.With("subsystem", "scheduler"),
		Version: buildinfo.Version, Started: started, Every: time.Minute}).Run(ctx)

	api := httpapi.New(db, notifier, []byte(cfg.AgentSharedKey), log.With("subsystem", "httpapi"), time.Now)
	api.LeaseSeconds = cfg.CommandLeaseSeconds
	api.AfterInventory = auto.Trigger
	autoRollback := &orchestrator.AutoRollback{Store: db, Notify: notifier, Log: log.With("subsystem", "auto-rollback"),
		Now: time.Now, ExpiryMinutes: cfg.CommandExpiryMinutes}
	api.AfterFinish = func(op store.Operation) {
		go autoRollback.OperationFinished(context.Background(), op)
	}
	handler := api.Handler()
	if addTestAPI != nil {
		handler = addTestAPI(router, handler)
	}
	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Error("listen", "addr", srv.Addr, "err", err)
		return exitError
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("http server listening", "addr", ln.Addr().String())

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		log.Error("http server stopped", "err", err)
		return exitError
	}

	// Stop taking Discord interactions, finish in-flight requests, send
	// queued notices, then close the database.
	if session != nil {
		session.Close()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("http shutdown", "err", err)
	}
	log.Info("stopped")
	return exitOK
}

// checkChannel warns when @everyone can see the notification channel.
func checkChannel(ctx context.Context, s *discord.Session, n *discord.Notifier, channelID string, log *slog.Logger) {
	public, err := s.EveryoneCanView(ctx, channelID)
	if err != nil {
		log.Warn("could not check notification channel permissions", "channel_id", channelID, "err", err)
		return
	}
	if public {
		log.Warn("the notification channel is visible to @everyone; make it private", "channel_id", channelID)
		n.WarnPublicChannel(ctx)
	}
}

// healthcheck probes the local /healthz. It reads only LISTEN_PORT so it
// works inside the container without touching secrets.
func healthcheck() int {
	port := os.Getenv("LISTEN_PORT")
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		slog.Error("healthcheck", "err", err)
		return exitError
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Error("healthcheck", "status", resp.StatusCode)
		return exitError
	}
	return exitOK
}

// backup writes a consistent copy of the database while the bot runs
// , for example:
//
//	docker exec ddum-bot ddum-bot backup /data/ddum-backup.db
//
// It reads only DATABASE_PATH, never secrets.
func backup(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: ddum-bot backup <path>")
		return exitConfig
	}
	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "/data/ddum.db"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := store.Backup(ctx, dbPath, args[0]); err != nil {
		fmt.Fprintln(os.Stderr, "ddum-bot backup:", err)
		return exitError
	}
	fmt.Println("backup written to", args[0])
	return exitOK
}

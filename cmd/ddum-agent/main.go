// Command ddum-agent runs on every managed Docker host.
//
// Usage:
//
//	ddum-agent              run the agent (same as "ddum-agent run")
//	ddum-agent healthcheck  exit 0 if the poll loop is alive
//	ddum-agent gen-id       print a new UUIDv7 for AGENT_ID
//	ddum-agent version      print the version
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/client"
	"github.com/DragonDrew/ddum/internal/agent/config"
	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/agent/executor"
	"github.com/DragonDrew/ddum/internal/agent/identity"
	"github.com/DragonDrew/ddum/internal/agent/portainer"
	"github.com/DragonDrew/ddum/internal/buildinfo"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/logging"
	"github.com/DragonDrew/ddum/internal/protocol"
)

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
	case "gen-id":
		fmt.Println(ids.V7{}.New())
	case "version", "--version", "-v":
		fmt.Println(buildinfo.Version)
	case "help", "--help", "-h":
		fmt.Println("usage: ddum-agent [run|healthcheck|gen-id|version]")
	default:
		fmt.Fprintf(os.Stderr, "ddum-agent: unknown command %q (want run, healthcheck, gen-id, or version)\n", cmd)
		os.Exit(exitConfig)
	}
}

func run() int {
	cfg, err := config.Load(config.Hostname())
	if err != nil {
		fmt.Fprintf(os.Stderr, "ddum-agent: invalid configuration:\n%v\n", err)
		return exitConfig
	}
	log := logging.New(os.Stdout, cfg.Level, "agent")
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}
	if cfg.GHCRToken != "" {
		// The docker commands the agent runs inherit DOCKER_CONFIG. The login
		// lives in the container's /tmp, never in a volume.
		base := filepath.Join(os.Getenv("HOME"), ".docker")
		if d := os.Getenv("DOCKER_CONFIG"); d != "" {
			base = d
		}
		dir := filepath.Join(os.TempDir(), "ddum-docker-config")
		if err := executor.RegistryLogin(dir, filepath.Join(base, "config.json"), cfg.GHCRToken); err != nil {
			log.Error("write registry login for GHCR_TOKEN", "err", err)
			return exitConfig
		}
		os.Setenv("DOCKER_CONFIG", dir)
	}

	id, err := identity.Resolve(cfg.DataDir, cfg.AgentID, ids.V7{})
	if err != nil {
		log.Error("resolve agent ID", "err", err)
		return exitError
	}
	if id.Warning != "" {
		log.Warn(id.Warning)
	}
	log = log.With("agent_id", id.ID)
	log.Info("starting", "version", buildinfo.Version, "agent_id_source", id.Source, "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	engine, err := docker.New(cfg.DockerHost)
	if err != nil {
		log.Error("docker client", "err", err)
		return exitError
	}
	defer engine.Close()

	journal, err := executor.OpenJournal(cfg.DataDir)
	if err != nil {
		log.Error("open journal", "err", err)
		return exitError
	}
	agentKey, err := identity.LoadKey(cfg.DataDir)
	if err != nil {
		log.Error("load agent key", "err", err)
		return exitError
	}
	botKey := cfg.BotPublicKey
	if botKey == "" {
		if botKey, err = identity.LoadBotKey(cfg.DataDir); err != nil {
			log.Error("load pinned bot key", "err", err)
			return exitError
		}
	}
	log.Info("agent key", "fingerprint", protocol.KeyFingerprint(protocol.EncodePublicKey(agentKey)), "bot_key_pinned", botKey != "")
	wake := make(chan struct{}, 1)
	loop := &client.Loop{
		HTTP: &client.HTTP{
			BaseURL:  cfg.BotURL,
			Key:      []byte(cfg.AgentSharedKey),
			AgentID:  id.ID,
			Client:   &http.Client{Timeout: 30 * time.Second},
			Now:      time.Now,
			AgentKey: agentKey,
			BotKey:   botKey,
			SaveBotKey: func(key string) error {
				log.Info("pinned the bot's key", "fingerprint", protocol.KeyFingerprint(key))
				return identity.SaveBotKey(cfg.DataDir, key)
			},
		},
		Collector: &discovery.Collector{
			Engine: engine,
			Options: discovery.Options{
				EnabledProviders:    cfg.EnabledProviders,
				ComposeAllowedRoots: cfg.ComposeAllowedRoots,
				Files:               discovery.OSFiles{},
			},
			Now: time.Now,
		},
		Log:               log,
		AgentVersion:      buildinfo.Version,
		HostName:          cfg.HostDisplayName,
		PollInterval:      time.Duration(cfg.PollIntervalSeconds) * time.Second,
		InventoryInterval: time.Duration(cfg.InventoryIntervalSeconds) * time.Second,
		Capabilities:      capabilities(cfg),
		DataDir:           cfg.DataDir,
		Now:               time.Now,
		Wake:              wake,
	}
	x := &executor.Executor{
		Journal: journal, Engine: engine, Runner: executor.ExecRunner{Binary: "docker"}, Files: discovery.OSFiles{},
		AllowedRoots: cfg.ComposeAllowedRoots, DataDir: cfg.DataDir, Inventory: loop.LatestStacks,
		Log: log.With("subsystem", "executor"), Now: time.Now, NewID: ids.V7{}.New, RetentionCount: cfg.SnapshotRetentionCount,
		Wake: func() {
			select {
			case wake <- struct{}{}:
			default:
			}
		},
	}
	loop.Ops = x
	collector := loop.Collector.(*discovery.Collector)
	collector.Rollback = x.AvailableSnapshots
	if cfg.HasProvider(config.ProviderPortainer) {
		pc, err := portainer.New(cfg.PortainerURL, cfg.PortainerToken, cfg.PortainerEndpointID, cfg.PortainerInsecureTLS, cfg.PortainerCAFile)
		if err != nil {
			log.Error("Portainer client", "err", err)
			return exitConfig
		}
		if cfg.PortainerInsecureTLS {
			log.Warn("PORTAINER_INSECURE_TLS accepts any certificate, so the Portainer token goes to whoever answers; PORTAINER_CA_FILE is safer")
		}
		collector.Portainer = pc
		x.Portainer = pc
		x.OnPortainerChange = collector.ForgetPortainerFile
		log.Info("Portainer provider enabled", "url", cfg.PortainerURL, "endpoint_id", cfg.PortainerEndpointID)
	}
	x.Recover() // operations cut short by a restart become interrupted

	// The executor gets its own context: SIGTERM stops polling, but a running
	// operation may finish its step within the stop grace period.
	execCtx, stopExec := context.WithCancel(context.Background())
	defer stopExec()
	go x.Run(execCtx)

	loop.Run(ctx)
	if x.Busy() {
		log.Warn("stopping while an operation runs; waiting up to 25 s for it to finish")
		for i := 0; i < 250 && x.Busy(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if x.Busy() {
			log.Warn("operation still running; it will be reported as interrupted after restart")
		}
	}
	log.Info("stopped")
	return exitOK
}

// capabilities advertises what this agent can do. Compose stacks can
// be deployed (including tag changes) and rolled back; so can Portainer
// stacks when PORTAINER_URL is set.
func capabilities(cfg *config.Config) []string {
	var caps []string
	if cfg.HasProvider(config.ProviderCompose) {
		caps = append(caps, protocol.CapProviderCompose)
	}
	if cfg.HasProvider(config.ProviderPortainer) {
		caps = append(caps, protocol.CapProviderPortainer)
	}
	if cfg.HasProvider(config.ProviderCompose) || cfg.HasProvider(config.ProviderPortainer) {
		caps = append(caps, protocol.CapActionDeploy, protocol.CapActionRollback, protocol.CapTagChangeEdit)
	}
	if cfg.HasProvider(config.ProviderDocker) {
		caps = append(caps, protocol.CapProviderDocker)
	}
	return caps
}

// healthcheck reports whether the poll loop is still running. It reads only
// DATA_DIR, never secrets.
func healthcheck() int {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "/data"
	}
	if err := client.CheckHeartbeat(dir, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "ddum-agent healthcheck:", err)
		return exitError
	}
	return exitOK
}

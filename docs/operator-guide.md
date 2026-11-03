# ddum operator guide

This guide covers running ddum after the [README quickstart](../README.md#setup): Discord setup in detail, adding hosts, Portainer, mounts, network exposure, secrets, labels, and backups. For problems, see [troubleshooting](troubleshooting.md).

## Contents

- [How the pieces fit](#how-the-pieces-fit)
- [Discord](#discord)
  - [The bot application](#the-bot-application)
  - [The notification channel must be private](#the-notification-channel-must-be-private)
  - [Who can do what](#who-can-do-what)
  - [Command visibility](#command-visibility)
- [More hosts](#more-hosts)
  - [An agent as a Portainer stack](#an-agent-as-a-portainer-stack)
- [Network exposure](#network-exposure)
- [Mounts and allowed roots](#mounts-and-allowed-roots)
- [Variables your stacks need](#variables-your-stacks-need)
- [Portainer](#portainer)
- [Secrets](#secrets)
- [Labels](#labels)
- [Scheduled deploys](#scheduled-deploys)
- [Private registries](#private-registries)
- [Updating ddum itself](#updating-ddum-itself)
- [Backups and retention](#backups-and-retention)
- [Renamed and moved stacks](#renamed-and-moved-stacks)
- [Configuration reference](#configuration-reference)

## How the pieces fit

```text
 Discord  ◄──── gateway ────►  ddum-bot  ◄──── HTTP, HMAC-signed ────  ddum-agent (one per Docker host)
                               SQLite DB                                Docker socket, your stack files
                               registry checks                          Portainer API (optional)
```

- **One bot.** It talks to Discord and the registries, stores everything in SQLite, and decides *what* should run.
- **One agent per Docker host.** It discovers stacks, reports them, and decides *how* to deploy what you approved. Agents poll the bot. The bot never connects to an agent, so agents need no open port.
- **An agent refuses anything unsafe,** whatever the bot asks for. It never runs a command built from strings the bot sent. It never deploys a container it did not discover itself, and never changes an image's repository. It accepts only images pinned by digest, never edits files outside `COMPOSE_ALLOWED_ROOTS`, and never touches a protected container. A compromised bot therefore cannot run arbitrary commands on your hosts. It could still ask for any published version of an image a container already uses. See the [security guide](security.md).

## Discord

### The bot application

The application needs:

| Setting | Value |
|---|---|
| OAuth2 scopes | `bot`, `applications.commands` |
| Bot permissions | View Channels, Send Messages, Embed Links, Read Message History (`84992`) |
| Privileged gateway intents | none |
| Public Bot | off, so nobody else can add it to their server |

The bot serves exactly one server (`DISCORD_GUILD_ID`) and posts to exactly one channel (`NOTIFICATION_CHANNEL_ID`). Slash commands are registered for that server at every startup, so changes appear immediately.

### The notification channel must be private

The channel shows host names, stack names, images, and digests: an inventory of what you run. **Make it private** and allow only the bot and the people who approve updates.

1. Channel settings → **Permissions** → turn on **Private Channel**, or deny **View Channel** to `@everyone`.
2. Add the bot, and each approver or a role holding them.

At startup the bot checks whether `@everyone` can view the channel. If so, it logs a warning and posts **🔓 This channel is public** once.

### Who can do what

- Only users listed in `AUTHORIZED_DISCORD_IDS` can do anything, including read-only commands such as `/stack list`. Everyone else gets "You are not authorized." This also applies to buttons: anyone in the channel can click one, but only an authorized user's click does anything.
- **Replies are cards in the channel.** A command run in the notification channel answers with a card there, so the other approvers see it too. Its buttons edit the same card, and ddum keeps editing it as things happen:
  - `/update check` shows "Checking…", then what is waiting for approval, with **Review**.
  - **Approve** turns an update card into "⏳ Deploy queued" with **Cancel**, then into the result with **Roll back**.
  - **Snooze**, **Roll back**, and the other confirmations happen on the card, and **Back**, **Keep**, or **Cancel** put it back.

  Errors, and every reply to a command run in another channel, stay private to the person who ran it. Cards are tracked in the database, so they keep updating across bot restarts, including a bot update's own result.
- Destructive actions ask for confirmation first. These are rollback, removing a host, deleting a group, merging stacks, unsnooze-all, and approving a bot update.
- Secrets are never accepted through Discord.

To change who is authorized, edit `AUTHORIZED_DISCORD_IDS` and run `docker compose up -d ddum-bot`.

### Command visibility

The commands are registered so that only server admins see them by default. This only hides them; the actual check is `AUTHORIZED_DISCORD_IDS`. To show the commands to an approver who is not an admin:

1. **Server Settings → Integrations →** your ddum app.
2. Under **Roles & Members**, add the approver, or a role they hold.

The person also needs to be in `AUTHORIZED_DISCORD_IDS`. Without it, they see the commands but every one of them is refused.

## More hosts

Run one agent on each additional Docker host. Agents connect to the bot, so the bot's port must be reachable from them (see [network exposure](#network-exposure)).

1. **Publish the bot's port on its LAN address.** In the bot's `compose.yaml`, add:

   ```yaml
       ports:
         - "192.168.1.10:8080:8080"   # the bot host's LAN IP, never 0.0.0.0 on an internet-facing host
   ```

2. **Give the new host the same agent key.** Copy the `AGENT_SHARED_KEY` value from the bot host's `.env` into a `.env` next to the new agent's `compose.yaml` (`chmod 600`), over a secure channel.
3. **Generate a new agent ID** on the new host with `docker run --rm ghcr.io/dragondrew/ddum-agent:0.10.0 gen-id`, and put it in that `.env` as `AGENT_ID`. Every host needs its own ID.
4. **Write the agent's `compose.yaml`:**

   ```yaml
   name: ddum-agent
   services:
     ddum-agent:
       image: ghcr.io/dragondrew/ddum-agent:0.10.0
       restart: unless-stopped
       labels:
         ddum.self: "agent"
       environment:
         BOT_URL: http://192.168.1.10:8080
         AGENT_SHARED_KEY: ${AGENT_SHARED_KEY:?set it in .env}
         AGENT_ID: ${AGENT_ID:?set it in .env}
         HOST_DISPLAY_NAME: second-host
         COMPOSE_ALLOWED_ROOTS: /opt/stacks
         STACK_ENV_HOME: ${HOME}   # see "Variables your stacks need"
       volumes:
         - /var/run/docker.sock:/var/run/docker.sock
         - ddum-agent-data:/data
         - /opt/stacks:/opt/stacks
   volumes:
     ddum-agent-data:
   ```

5. **Start it** with `docker compose up -d`, then **approve the host** from the message in the channel.

### An agent as a Portainer stack

On a host that Portainer manages, including one that runs only `portainer_agent`, you can deploy the agent from Portainer instead.

**How it works:** one ddum agent per Docker host manages that host's containers. The agent does not need to run on the Portainer server: it calls the server's API for stacks on its own host.

1. **Open the host in Portainer:** in the Portainer that manages that host, open the host's environment and go to **Stacks → Add stack**.
2. **Paste this stack:**

   ```yaml
   services:
     ddum-agent:
       image: ghcr.io/dragondrew/ddum-agent:0.10.0
       restart: unless-stopped
       labels:
         ddum.self: "agent"
       environment:
         BOT_URL: http://192.168.1.10:8080           # the bot's LAN address
         AGENT_SHARED_KEY: ${AGENT_SHARED_KEY}
         AGENT_ID: ${AGENT_ID}
         HOST_DISPLAY_NAME: ${HOST_DISPLAY_NAME}
         PORTAINER_URL: ${PORTAINER_URL}             # the Portainer server, e.g. https://192.168.1.20:9443
         PORTAINER_TOKEN: ${PORTAINER_TOKEN}
         PORTAINER_ENDPOINT_ID: ${PORTAINER_ENDPOINT_ID}
         PORTAINER_INSECURE_TLS: "true"             # Portainer's self-signed certificate; see "Portainer" for the safer way
       volumes:
         - /var/run/docker.sock:/var/run/docker.sock
         - ddum-agent-data:/data
   volumes:
     ddum-agent-data:
   ```

3. **Fill in the stack's Environment variables in Portainer:**
   - `AGENT_SHARED_KEY`: the same key as the bot's.
   - `AGENT_ID`: a new UUID for this host, from `docker run --rm ghcr.io/dragondrew/ddum-agent:0.10.0 gen-id` or PowerShell's `[guid]::NewGuid().ToString()`.
   - `HOST_DISPLAY_NAME`: the name shown in Discord.
   - `PORTAINER_URL`, `PORTAINER_TOKEN`, and `PORTAINER_ENDPOINT_ID`: the Portainer server, an access token from it, and this host's environment ID. The ID is the number in Portainer's URL when the environment is open.
4. **Deploy, then approve:** deploy the stack, then approve **New host wants to register** in Discord.

**Worth knowing:**
- **No stack folders to mount:** Portainer keeps the stack files on its server, so the agent needs no stack folders. Mount a folder (at the same path) and set `COMPOSE_ALLOWED_ROOTS` only for plain Compose projects kept on that host.
- **Variables are visible in Portainer:** Portainer stores the stack's variables and shows them to its admins.
- **Upgrading the agent:** the agent never updates itself. To upgrade it, change the image tag in the stack in Portainer.
- **`portainer_agent` itself:** ddum lists it but never redeploys it, because it is not part of a stack. Label it `ddum.protected=true` or `ddum.ignore=true` to silence its update notices.

A host that registers stays `pending` and gets no commands until someone approves it. Each host's name is only a label; you can change it later with `/host rename`.

To retire a host, run `/host remove`. To move a host's identity to a reinstalled machine with a new agent ID, run `/host reassign-agent`.

## Network exposure

**The agent-to-bot protocol is plain HTTP.** Every request and response is signed, so it cannot be forged or altered, but it is not encrypted: anyone on the path can read your inventory.

- **One host:** publish no port at all. The agent reaches the bot as `http://ddum-bot:8080` over the Compose network. This is the quickstart's setup.
- **Several hosts on one LAN:** bind the published port to the bot host's **LAN address**, such as `192.168.1.10:8080:8080`. A bare `8080:8080` listens on every interface, including any public one.
- **Never port-forward or expose the port to the internet.**
- **Hosts on different networks:** connect them over a private overlay, such as WireGuard or Tailscale, and bind to that interface. ddum has no built-in TLS, though a reverse proxy can add it as long as it doesn't change request paths: `BOT_URL` accepts `https://`.

Requests carry a timestamp and are refused if it is more than 5 minutes off the bot's clock, so keep every host's clock in sync (see [troubleshooting](troubleshooting.md#clock-problem)).

## Mounts and allowed roots

To deploy a Compose stack, the agent edits the image tag in its files and runs `docker compose` against them. For this it needs to see the files **at the same path as on the host**. Compose records the host path of each project, and the agent uses that path.

```yaml
    environment:
      COMPOSE_ALLOWED_ROOTS: /opt/stacks,/srv/compose
    volumes:
      - /opt/stacks:/opt/stacks       # same path on both sides
      - /srv/compose:/srv/compose     # read-write: the agent edits tags here
```

- **Allowed roots.** `COMPOSE_ALLOWED_ROOTS` lists the directories the agent may edit, comma-separated. It checks every file after resolving symlinks, and refuses one outside these directories.
- **Read-write mounts.** The agent changes only the one tag value, either in the YAML or in a `.env` line. It keeps the rest of the file byte for byte and takes a snapshot first.
- **Stacks outside the allowed roots** are still listed and checked for updates. They show as Held, with the reason "manual update required".
- **Dockge and similar tools,** which keep plain Compose files in a directory, need nothing special: list that directory as a root and mount it.
- **Standalone containers,** started with `docker run`, are listed and checked but never deployed; update them by hand.
- **Every file a stack uses** must be visible to the agent at the same path, including an `env_file` or `include` outside the project folder. Otherwise ddum refuses to deploy it, and says which file. Bind-mount sources are the exception: Docker resolves those on the host, so they can be anywhere.

## Variables your stacks need

The agent runs `docker compose` in its own container, so it has neither your shell nor your home folder. Compose fills in a stack's variables from its environment first, then from `.env`. So a stack that relies on your shell, such as `~/data:/data`, `${HOME}/data:/data` or a variable you exported, would come back different if the agent recreated it as things stand.

**ddum checks this before every Compose deploy.** If recreating a service would change its mounts or variables, it refuses, changes nothing, and names what would change. The agent's own settings and secrets never reach Compose.

To give stacks what they need, add `STACK_ENV_<NAME>` to the agent, and Compose sees `<NAME>`:

```yaml
    environment:
      STACK_ENV_HOME: ${HOME}       # ~ and ${HOME} resolve as they do for you
      STACK_ENV_PUID: "1000"        # anything else you export in your shell
```

`${HOME}` here is filled in when you start the agent, with your own home folder. Better still, put a stack's values in its own `.env`, so nothing depends on who starts it.

Portainer stacks aren't affected: Portainer runs Compose itself, with the stack's own variables.

## Portainer

Stacks that Portainer manages are updated through the Portainer API, so the agent needs no access to their files.

1. **Create an access token** in Portainer: **My account → Access tokens → Add access token**. Use a user that can update the stacks.
2. **Find the environment ID:** open the environment in Portainer; the number in the URL (`#!/3/docker/...`) is the ID.
3. **Put the token and the environment ID in that host's `.env`** as `PORTAINER_TOKEN` and `PORTAINER_ENDPOINT_ID`. They belong to that host's agent only; the bot never sees them.
4. **Add to the agent:**

   ```yaml
       environment:
         PORTAINER_URL: https://portainer.lan:9443
         PORTAINER_TOKEN: ${PORTAINER_TOKEN:?set it in .env}
         PORTAINER_ENDPOINT_ID: ${PORTAINER_ENDPOINT_ID:?set it in .env}
         PORTAINER_INSECURE_TLS: "true"   # only for Portainer's self-signed certificate
   ```

   `PORTAINER_CA_FILE` is the safer way to use a self-signed certificate: mount the certificate into the agent and name the file. `PORTAINER_INSECURE_TLS` accepts any certificate, so the token goes to whoever answers, and the agent logs a warning.

   If Portainer runs on the same host and is published only on the host, use `PORTAINER_URL: https://host.docker.internal:9443` and add `extra_hosts: ["host.docker.internal:host-gateway"]` to the agent.

**What happens on deploy:**

- **File-based stacks:** the agent edits the tag in the stack file, or in the stack's environment variables, and has Portainer redeploy the stack. Portainer recreates only the services whose image changed.
- **Git-based stacks:**
  - updates that keep the same tag, such as `latest` getting a new digest, are deployed with a Git redeploy;
  - **tag changes are held,** because the file lives in your Git repository. Change the tag there; the message links the release notes.

Label Portainer itself `ddum.protected=true` so ddum never stops it.

## Secrets

Tokens, keys, and IDs go in a `.env` file next to `compose.yaml`, and `compose.yaml` references them as `${NAME}`. Compose reads `.env` from the project directory on its own, and substitutes each value only into the services that reference it, so every container gets only its own. Keep `.env` at `chmod 600` and out of any Git repository.

If you prefer Docker or Compose secrets, every secret variable also has a `_FILE` variant naming a file; if both are set, the file wins. These secrets never appear in logs, the database, or Discord. Values from your stacks' env files and Portainer variables are masked in failure output. A secret written straight into a Compose file can't be told from the rest of the file, so an error that quotes it can reach Discord. Keep secrets in env files.

| Secret | Who needs it | Variable |
|---|---|---|
| Discord bot token | the bot only | `DISCORD_TOKEN` |
| Agent shared key (≥ 32 characters; `openssl rand -hex 32`), which introduces a new agent | the bot **and** every agent | `AGENT_SHARED_KEY` |
| Portainer access token | only the agent on that Portainer's host | `PORTAINER_TOKEN` |
| GitHub token for `ghcr.io` and `lscr.io` (optional) | the bot, and any agent whose pulls should be logged in | `GHCR_TOKEN` (see [private registries](#private-registries)) |
| Other registry credentials | the bot, and the agent on any host that pulls those images | `DOCKER_CONFIG` (see [private registries](#private-registries)) |

Values from `.env` are ordinary environment variables, so anyone who can run `docker inspect` on the host can read them; that is anyone with Docker access, who could take over the host anyway.

**The shared key introduces new agents.** Anyone with it who can reach the bot can make a new host appear, but it gets nothing until someone approves it in Discord, and they can't pose as a host that already has a key.

**Each agent also has a key of its own,** made on first start and kept in its data volume, and the bot pins it at first contact. If an agent's key changes, Discord asks whether to accept the new one. See the [security guide](security.md#keys).

To rotate the shared key, change it on the bot and restart it. Existing agents don't use it any more, so only agents you add later need the new one.

## Labels

Labels are hints that ddum reads from your containers. Policies and groups are otherwise managed in Discord.

| Label | Effect |
|---|---|
| `ddum.ignore=true` | Listed, but never checked or updated. Overrides any policy. |
| `ddum.protected=true` | Never stopped or recreated by ddum. Use it for Portainer, databases you update by hand, and the like. |
| `ddum.group=<name>` | Puts the container in that update group when it is first seen. Removing it later in Discord is respected. |
| `ddum.stack=<name>` | Groups containers that have no Compose project into one stack. |
| `ddum.self=bot` / `ddum.self=agent` | Marks ddum's own containers. Required: see the quickstart. |

## Scheduled deploys

**Schedule** on an update card approves the updates now and deploys them later. Pick a whole hour in the next 24, such as 03:00, when nobody is using the app. The card turns into **⏰ Scheduled**, with **Cancel**, and becomes the result after the deploy.

- **Time zone:** the menu lists times in the bot's time zone. Set `TZ` on the bot to yours, such as `TZ: Australia/Sydney`. The card shows the chosen time in each viewer's own time zone.
- **Exact digest:** the deploy uses the digest you approved, even if the tag moves before then.
- **Queueing:** a scheduled deploy does not hold up other deploys on the host. At its time it waits for any deploy already running, then starts on the agent's next poll.
- **Expiry:** if the agent is offline at that time, the deploy expires after `COMMAND_EXPIRY_MINUTES` (60), counted from the scheduled time, and its updates become pending again.
- **Restarts:** scheduling survives restarts of the bot and agent. Maintenance mode does not pause a scheduled deploy, because it is a manual approval.

## Private registries

The bot checks registries and needs registry credentials, and each agent needs them to pull. The agent runs in a container, so it doesn't see your host's Docker login unless you mount it.

### ghcr.io and lscr.io

Anonymous requests to `ghcr.io` are rate limited, and `lscr.io` (LinuxServer images) gets its tokens from `ghcr.io`, so checks of many such images can be refused with "rate limited by registry". A GitHub token raises the limit:

1. Create a **classic** personal access token at github.com → Settings → Developer settings → Personal access tokens → Tokens (classic), with only `read:packages`. Fine-grained tokens don't work with `ghcr.io`.
2. Put it in `.env` as `GHCR_TOKEN`, and pass it to the bot and to each agent whose pulls should be logged in:

   ```yaml
       environment:
         GHCR_TOKEN: ${GHCR_TOKEN}
   ```

The token is used for `ghcr.io` and `lscr.io` only. The bot and agents log `ghcr_token_set=true` at startup. A wrong or expired token makes checks of those images fail with `DENIED`; replace it and recreate the containers.

### Other registries

1. On each host, log in to the registry with a config directory of its own:

   ```sh
   docker --config /opt/stacks/ddum/docker-config login registry.example.com
   ```

2. Mount that directory into the bot and into each agent that pulls those images, and point `DOCKER_CONFIG` at it:

   ```yaml
       environment:
         DOCKER_CONFIG: /config/docker
       volumes:
         - ./docker-config:/config/docker:ro
   ```

Without the bot's login, the image shows as untracked ("not found in its registry"). Without the agent's, the update is found but its deploy fails with `pull_failed`, and nothing changes.

## Updating ddum itself

- **The bot** is updated like any other container, but never automatically. Its update arrives as its own **🤖 ddum bot update available** message. Approving it asks for confirmation, because the bot restarts. The agent on its host deploys it. If the new bot does not come up healthy, the agent restores the previous one.
- **Agents** are protected and never update themselves. Update them by hand: change the image tag in the agent's `compose.yaml` and run `docker compose up -d`.

For the bot to be updatable, its `compose.yaml` must be under one of its agent's `COMPOSE_ALLOWED_ROOTS`, and it must run a published image pinned to a version (`ghcr.io/dragondrew/ddum-bot:0.10.0`, not `latest`). ddum can't see updates for an image you built locally.

**Upgrading across releases:** update the bot first, then each agent. Read the [changelog](../CHANGELOG.md) first, for anything a release needs from you. Each release is signed: the [release workflow](../.github/workflows/release.yml) has the command to check a signature.

## Backups and retention

### Backups

Everything the bot knows lives in one SQLite file, `/data/ddum.db`, in the `ddum-bot-data` volume, including the private key agents know the bot by. Keep backups private. Back it up while the bot runs:

```sh
cd /opt/stacks/ddum
docker compose exec ddum-bot ddum-bot backup /data/backup-$(date +%F).db
docker compose cp ddum-bot:/data/backup-$(date +%F).db ./
```

`ddum-bot backup <path>` writes a consistent copy (SQLite `VACUUM INTO`) and refuses to overwrite an existing file. Copy the backup off the host. The image has no shell, so remove old backups from the volume with a helper container:

```sh
docker run --rm -v ddum_ddum-bot-data:/data alpine rm /data/backup-2026-01-01.db
```

**To restore:**

```sh
docker compose stop ddum-bot
docker run --rm -v ddum_ddum-bot-data:/data -v "$PWD":/backup alpine sh -c \
  'cp /backup/backup-2026-01-01.db /data/ddum.db && rm -f /data/ddum.db-wal /data/ddum.db-shm && chown 65532:65532 /data/ddum.db'
docker compose start ddum-bot
```

Before a schema change, the bot also copies its database to `/data/ddum.db.before-schema-<version>`.

If you lose the bot's database and have no backup, agents refuse the new bot, because its key has changed. Delete `bot_key` from each agent's data volume, restart it, and approve the hosts again.

Agents keep their key, journal and rollback snapshots in `ddum-agent-data`. Those volumes don't need backing up. If one is lost, the agent comes back as the same host because `AGENT_ID` is pinned in `compose.yaml`, with a new key, so Discord asks you to accept it. Rollback to deployments made before the loss is gone.

### Retention

- **Kept forever:** audit events, operations, updates, desired states, and snoozes.
- **Deleted after 180 days:** the step-by-step events of an operation. A daily job removes them; the operation itself and its outcome stay.
- **On the agents:** the agent's journal is kept for 7 days. Rollback snapshots are kept for the last 3 deployments of each stack (`SNAPSHOT_RETENTION_COUNT`).
- **Never deleted:** hosts, stacks, and containers that disappear are marked missing or removed, so their history and policies survive if they come back.

## Renamed and moved stacks

ddum identifies a Compose stack by its project name. If you rename a project, or move it to another host, ddum sees a new stack, and the old one goes missing. When a new stack shares at least 80 % of its service names with a stack that is missing on the same host, ddum posts **🔀 Was <old> renamed?** with a **Merge…** button.

Merging keeps the older stack's identity: its policies, groups, snoozes, and history. It takes over the new stack's name, host, and containers. A merge cannot be undone, so it asks for confirmation. You can also merge by hand with `/stack merge source:<stack> into:<stack>`.

A stack that is missing for more than 24 hours (`STACK_MISSING_GRACE_HOURS`) has its pending updates closed. If it comes back, ddum checks it again.

## Configuration reference

### Bot

| Variable | Default | Notes |
|---|---|---|
| `DISCORD_TOKEN` / `_FILE` | required | Secret |
| `DISCORD_APPLICATION_ID` | required | |
| `DISCORD_GUILD_ID` | required | Your server |
| `NOTIFICATION_CHANNEL_ID` | required | A private channel |
| `AUTHORIZED_DISCORD_IDS` | required | Comma-separated user IDs |
| `AGENT_SHARED_KEY` / `_FILE` | required | Secret, ≥ 32 characters |
| `DATABASE_PATH` | `/data/ddum.db` | Never on a network share |
| `LISTEN_ADDRESS` / `LISTEN_PORT` | `0.0.0.0` / `8080` | Inside the container; restrict where the port is published |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `REGISTRY_CHECK_INTERVAL` | `6h` | Go duration |
| `REGISTRY_CONCURRENCY_PER_HOST` | `4` | Parallel requests per registry |
| `DOCKER_CONFIG` | unset | Directory with registry `config.json` |
| `GHCR_TOKEN` / `_FILE` | unset | GitHub token (classic, `read:packages`) for `ghcr.io` and `lscr.io` |
| `COMMAND_LEASE_SECONDS` | `120` | How long an agent holds a command before it can be handed out again |
| `COMMAND_EXPIRY_MINUTES` | `60` | A command not started by then expires |
| `STACK_MISSING_GRACE_HOURS` | `24` | |
| `NOTIFY_INDIVIDUAL_LIMIT` | `5` | More new stacks than this in one check are posted as a single summary |
| `TZ` | `UTC` | The time zone of the times **Schedule** offers, for example `Australia/Sydney`; storage is always UTC |

### Agent

| Variable | Default | Notes |
|---|---|---|
| `BOT_URL` | required | `http://ddum-bot:8080` on the bot's host, otherwise its LAN address |
| `BOT_PUBLIC_KEY` | learned on first contact | The bot's public key, to pin it from the start |
| `AGENT_SHARED_KEY` / `_FILE` | required | Same key as the bot |
| `AGENT_ID` | generated | Pin it: `ddum-agent gen-id` |
| `HOST_DISPLAY_NAME` | hostname | Renamable in Discord |
| `POLL_INTERVAL_SECONDS` | `30` | 3–300 |
| `INVENTORY_INTERVAL_SECONDS` | `300` | Full inventory at least this often; changes are sent on the next poll |
| `DATA_DIR` | `/data` | Journal, snapshots |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | |
| `ENABLED_PROVIDERS` | `portainer,compose,docker` | |
| `COMPOSE_ALLOWED_ROOTS` | unset | Required for Compose deploys |
| `STACK_ENV_<NAME>` | unset | Reaches `docker compose` as `<NAME>`. See [variables your stacks need](#variables-your-stacks-need) |
| `PORTAINER_URL`, `PORTAINER_TOKEN` / `_FILE`, `PORTAINER_ENDPOINT_ID` | unset | Set all three to use the Portainer provider |
| `PORTAINER_CA_FILE` | unset | PEM file of certificates to trust |
| `PORTAINER_INSECURE_TLS` | `false` | Accepts any certificate |
| `DOCKER_CONFIG` | unset | Directory with registry `config.json` for pulls |
| `GHCR_TOKEN` / `_FILE` | unset | Logs pulls from `ghcr.io` and `lscr.io` in |
| `SNAPSHOT_RETENTION_COUNT` | `3` | Per stack |
| `LOG_LEVEL` | `info` | |

A host is shown **offline** once it has not polled for 90 seconds, or 3 poll intervals if that is longer.

## Limits

- **Shared tag variables:** a tag set by one variable that several services use, as in the official Immich Compose file, isn't changed for you. Give each service its own variable or a literal tag. Digest updates of the same tag still work.
- **Multi-replica services** and **standalone `docker run` containers** are listed and checked, but you update them yourself.
- **32-bit ARM** hosts can't be approved.
- **Swarm and Kubernetes** aren't supported.
- **Roles:** everyone you authorize can do everything.
- **One Discord server** and one channel per bot.

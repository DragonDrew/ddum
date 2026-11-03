# Changelog

What changed in each release, and what to do about it.

## 0.10.1 (2026-11-03)

A maintenance release. Nothing in how ddum behaves has changed.

### Changes

- **Dependencies:** the Docker API client, the database driver, the migrations library and the config parser are updated to their current releases.
- **Build and release tooling:** the GitHub Actions and Docker actions used by CI and releases are updated to their current major versions.
- **Release workflow:** a tag with a suffix, such as `v0.10.1-rc.1`, now runs as a pre-release. It never moves `:latest`.

## 0.10.0 (2026-11-03)

The first public release, as a beta. Mostly safety and security.

**Upgrading from 0.9:** update the bot first, then each agent. Agents on 0.9 keep working until you do. Each agent's key is pinned when it first connects on 0.10. If your Compose stacks use `~`, `${HOME}`, or variables from your shell, read [the first item below](#deploys-cant-quietly-change-a-stack): those stacks now need `STACK_ENV_` settings on their agent, and a deploy is refused until then. Stacks managed through Portainer aren't affected.

### Deploys can't quietly change a stack

- **A Compose deploy could move a stack's data.** The agent ran `docker compose` with its own environment, so `~/data` or `${HOME}/data` could come back mounted from `/root/data`, with an empty folder, while the card said ✅ Deployed. Variables from your shell were lost, and a rollback didn't undo it. Now the agent checks first that Compose would recreate the service as it runs now, and refuses, naming what would change, if it wouldn't. Give a stack what it needs with `STACK_ENV_<NAME>` on the agent.
- **A rollback lost edits made since the deploy.** It wrote back whole copies of the Compose file and `.env`, or a Portainer stack's file and variables, so a rotated password went back to its old value. A rollback now changes the tag and nothing else.
- **A stack Compose can't load from the agent**, such as one with an `env_file` outside the mounted folders, is refused before anything changes, with the reason on the card. If `docker compose up` fails and no container changed, the file edit is undone.
- **A refused rollback** no longer holds the stack, since nothing changed.
- **A check right after a rollback could lose an update**, so an update group could be offered, and deployed, with a member missing. A check now waits for hosts that just finished an operation to report what they run.

### Security

- **Every agent has its own key, and so does the bot.** The shared key used to do everything, so root on one host could pose as another host's agent, and with a position on the network forge commands. It now only introduces a new agent. Keys are pinned on first contact, and a key change asks in Discord.
- **Someone on the network could forge parts of a response** with no key at all, enough to park an agent for a week or make it discard a deploy's result. Responses now cover their status and `Retry-After`, and a request that fails verification gets no signature. `Retry-After` is capped at 5 minutes.
- **Requests can't be replayed**: each carries a nonce.
- **Stacks can no longer read the agent's secrets** through variables such as `${AGENT_SHARED_KEY}`.
- **Snapshots no longer keep copies of your files**, which could hold secrets long after you rotated them. Copies from earlier versions are deleted when the agent starts.
- **Failure output hides values from env files and Portainer variables** before it reaches the database or Discord.
- **An image's own labels are no longer instructions**, so an image can't hide its own updates with `ddum.ignore`, or take over another stack by claiming its Compose labels.
- **The test mode with a fake Discord is gone from release images.** It let anyone with the shared key act as any Discord user, if enabled.
- **The database is private to the bot**, including one an earlier version made, and backups too. It's copied before any schema change.
- **`PORTAINER_CA_FILE`** trusts Portainer's certificate properly. `PORTAINER_INSECURE_TLS` now warns.
- A panic in a Discord handler no longer stops the bot. Requests that fail verification are logged at most once a minute per address, and a large unsigned body is refused sooner.

### New

- **`linux/arm64`:** images are published for it, and arm64 hosts can be approved. They're offered the newest version with a build for them.
- **Local builds on Docker's containerd image store** are recognised. They used to fail every check. On Docker 29.1, a running image whose tag moved no longer looks like a local build and silently stops getting updates. A new update of an image its repository doesn't hold waits for a person, and the card warns that it may be a local build.
- **Versions outside `tag_track` are mentioned:** a calendar-versioned image used to stop at the end of the year in silence. `/container show` and update cards now say a newer version exists. It's never deployed without you changing the policy.
- **Private registries:** the guide now covers logging agents in as well as the bot.

### Fixed

- The docs installed an old version, and said some things that weren't true: see the [operator guide](docs/operator-guide.md).

## 0.9.11 (2026-11-02)

A failed automatic deploy is no longer retried without end. ddum follows changes made by hand instead of offering to undo them. Operations that can't finish no longer block their host for good. Works with Docker's containerd image store. Edits keep a file's owner and mode.

## 0.9.10 (2026-10-29)

Daemon "not found" answers are recognised with the current Docker client, and a failed discovery no longer marks a host's stacks as missing.

## 0.9.9 (2026-10-27)

Same-version builds on `ghcr.io` and `lscr.io` are ordered by when they were pushed, which follows a publisher restarting its build counter.

## 0.9.8 (2026-10-25)

Fixed the remaining `ghcr.io` rate limiting, by extending tag lists and storing them. Rate-limited checks are retried in the same cycle. Checks failing for a day, and agents on a different version from the bot, are reported in Discord.

## 0.9.7 (2026-10-15)

`GHCR_TOKEN` logs the bot and agents in to `ghcr.io` and `lscr.io`.

## 0.9.6 (2026-10-15)

Less registry traffic: one login per registry, paced requests, and tag lists reused while `latest` stays put.

## Earlier

0.9.5 and before were early development.

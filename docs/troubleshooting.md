# Troubleshooting ddum

Start with the logs. They are structured JSON and never contain secrets:

```sh
docker compose logs --tail 100 ddum-bot
docker compose logs --tail 100 ddum-agent
```

For more detail, set `LOG_LEVEL: debug` on the service and run `docker compose up -d`. In Discord, `/system status` shows the bot version, uptime, agents online and offline, the last registry check, and maintenance mode.

## Contents

- [The bot does not start](#the-bot-does-not-start)
- [Slash commands do not appear](#slash-commands-do-not-appear)
- ["You are not authorized."](#you-are-not-authorized)
- [Nothing is posted in the channel](#nothing-is-posted-in-the-channel)
- [The host never shows up](#the-host-never-shows-up)
- [A host presents a different key](#a-host-presents-a-different-key)
- [Agents refuse the bot](#agents-refuse-the-bot)
- [Clock problem](#clock-problem)
- [A host is offline](#a-host-is-offline)
- [Agent version differs](#agent-version-differs)
- [No updates are found](#no-updates-are-found)
- [An update is Held](#an-update-is-held)
- [A deploy failed](#a-deploy-failed)
- [Recreating a service "would change it"](#recreating-a-service-would-change-it)
- [A rollback failed and the stack is held](#a-rollback-failed-and-the-stack-is-held)
- [A bot update failed](#a-bot-update-failed)
- [A stack went missing or appeared twice](#a-stack-went-missing-or-appeared-twice)
- [Changed outside ddum](#changed-outside-ddum)
- [Database problems](#database-problems)

## The bot does not start

The bot refuses to start when its configuration is wrong. It logs the variable at fault and exits; `docker compose ps` shows it restarting.

- **A required variable is missing:** `DISCORD_TOKEN`, `DISCORD_APPLICATION_ID`, `DISCORD_GUILD_ID`, `NOTIFICATION_CHANNEL_ID`, `AUTHORIZED_DISCORD_IDS`, or `AGENT_SHARED_KEY`.
- **The shared key is too short:** it must be at least 32 characters. Use `openssl rand -hex 32`.
- **`required variable … is missing a value`** from `docker compose`: that value is missing from `.env`, or `.env` is not next to `compose.yaml`. Run `docker compose config` to check what Compose substitutes.
- **A `_FILE` path does not exist in the container** (if you use secret files): check the `secrets:` entries, both at the top level and under the service. The bot runs as a non-root user, so its secret files must be readable (`chmod 644`).
- **The Discord token is wrong or was reset:** the log shows an authentication error from Discord. Put the current token in `.env` and run `docker compose up -d`.

## Slash commands do not appear

- Commands are registered for one server only. Check that `DISCORD_GUILD_ID` is that server's ID.
- The bot must have been invited with the `applications.commands` scope. If in doubt, re-invite it with the URL from the [README](../README.md#setup); this does not remove it first.
- Commands are hidden from members who are not admins. See [command visibility](operator-guide.md#command-visibility).
- Discord caches commands. Restart your Discord client, or press Ctrl+R in the desktop app.

## "You are not authorized."

Your Discord user ID is not in `AUTHORIZED_DISCORD_IDS`. Copy your ID (Developer Mode → right-click your name → **Copy User ID**), add it with a comma, and run `docker compose up -d ddum-bot`.

## Nothing is posted in the channel

- **Check the channel ID.** `NOTIFICATION_CHANNEL_ID` must be the channel's ID, not the server's.
- **Check the bot's permissions.** The bot needs **View Channel**, **Send Messages**, **Embed Links**, and **Read Message History** in that channel. A private channel must list the bot, or a role it has.
- **Check maintenance mode.** With `/maintenance status` on, update notifications are paused. Turn it off with `/maintenance off`, and ddum posts one summary of what is pending.
- **Read the log.** A failed post is logged as `post notice`, with Discord's error.

## The host never shows up

The new host should appear as **New host wants to register**. If it doesn't, look at the agent's log:

| Log says | Fix |
|---|---|
| connection refused, no such host, timeout | `BOT_URL` is wrong, or the port is not published or reachable. On the bot's host use `http://ddum-bot:8080`. From another host, use the bot host's LAN address and check the [published port](operator-guide.md#network-exposure). |
| "response signature missing or invalid (HTTP 401)" | The agent and the bot have different `AGENT_SHARED_KEY`s. Copy the bot's key exactly; a trailing newline is fine. |
| request expired, timestamp | See [clock problem](#clock-problem). |

If the host shows up but nothing happens after that, check `/host list`. A host that is `pending` gets nothing until someone clicks **Approve**. A host on an architecture other than `amd64` or `arm64` cannot be approved.

## A host presents a different key

**⚠️ <host> presents a different key** means requests for that host are signed with a key other than the one the bot pinned, so they're refused until someone decides.

The usual cause is that the agent's data volume was lost or recreated. Before you accept, compare the fingerprint on the card with the `agent key` line in the agent's log on that host:

```sh
docker compose logs ddum-agent | grep '"agent key"'
```

If they match, click **Accept new key**. If they don't, or you can't explain it, click **Keep the old key** and find out what's presenting it. It could be someone else who has the shared key.

## Agents refuse the bot

An agent logging "the bot's key is not the one this agent pinned" has been answered by a different key. If you reinstalled the bot without restoring its database, delete `bot_key` from each agent's data volume and restart it, then approve the hosts again. If you didn't, something else is answering as the bot, so check `BOT_URL` and your network first.

## Clock problem

**⏰ Clock problem on <host>** means the bot refused a correctly signed request because its timestamp was more than 5 minutes away from the bot's clock. The notice says how far off the clock is and in which direction. It is shown at most once an hour per agent. Until the clock is fixed, that host cannot report or receive anything, and soon shows as offline.

Fix the clock on whichever machine is wrong, the agent's host or the bot's:

```sh
timedatectl status                  # "System clock synchronized: yes" is what you want
sudo timedatectl set-ntp true       # turn on NTP sync (systemd-timesyncd)
```

Without systemd, install and enable `chrony` or `ntpd`. Containers use the host's clock, so restarting containers doesn't help. On a virtual machine, check that the hypervisor's time sync is on, and remember that a paused or restored VM can come back with a stale clock.

The time zone doesn't matter. ddum signs and compares UTC; `TZ` only changes how times are displayed.

## A host is offline

**🔴 <host> is offline** is posted when a host has not polled for 90 seconds, or 3 poll intervals if that is longer. **🟢 <host> is back online** is posted when it polls again.

While the host is offline:

- its updates are Held ("agent offline");
- `/stack list` still shows its last inventory, with its age;
- commands queued for it wait. If it does not pick one up within `COMMAND_EXPIRY_MINUTES` (60), the command expires: **⌛ Deploy expired**. Nothing ran, and the updates are pending again.

Check the agent with `docker compose ps` and `docker compose logs ddum-agent` on that host. Clock trouble also shows up as an offline host.

## Agent version differs

ddum never updates its agents (they are protected), so after a release you update each one yourself. The bot posts this notice for each host whose agent runs a different version from the bot. To fix it, set the `ddum-agent` image tag to the bot's version in that host's stack, for example in Portainer, and redeploy the stack. Each version difference is reported once per host. `/host show` shows the agent version.

## No updates are found

- **Run a check now** with `/update check`. Otherwise ddum checks every `REGISTRY_CHECK_INTERVAL` (6 hours). Scheduled checks reuse a repository's tag list while its `latest` tag has not moved, so a release that leaves `latest` alone (a patch on an older version line) can take up to a day to appear. `/update check` always lists tags afresh. This delay does not apply to images on ghcr.io and lscr.io, whose tag lists are brought up to date on every check.
- **Look at the container** with `/container show`. It shows the tracking mode and the last check error, if any.

| Shown | Meaning |
|---|---|
| `untracked`, pinned by digest | The image is `name@sha256:…`. ddum never moves a pinned image. |
| `untracked`, no registry digest | The image was built or loaded locally, so there is nothing to compare against. This includes ddum's own images if you built them yourself. |
| `untracked`, not found in its registry | No check has ever found this image. It's a local build (on Docker's containerd image store they look like registry images), or a private image the bot has no login for. See [private registries](operator-guide.md#private-registries). |
| `untracked`, ignored | The `ddum.ignore=true` label, or `mode: ignore` in its policy. |
| A last check error | The registry refused or failed. Common causes: Docker Hub, `ghcr.io` or `lscr.io` rate limits (log in or set `GHCR_TOKEN`, see [private registries](operator-guide.md#private-registries)), `DENIED` from `ghcr.io` or `lscr.io` (`GHCR_TOKEN` is wrong or expired), a private image without credentials, or a tag that no longer exists. It is retried at the next check. After a day of failures, ddum posts a **Registry checks failing** notice. |

- **Version tags follow the `tag_track` policy.** The default is `same-major`, so `1.27.0` is offered `1.29.3` but not `2.0.0`, and a calendar version such as `2026.12.1` is not offered `2027.1.0`. `/container show` mentions a newer version it leaves out. Change it with `/policy set tag_track:any`. Pre-releases are never offered. Suffixes must match: `1.2.3-alpine3.20` is only offered other `-alpine3.20` tags. See the effective policy with `/policy show`.
- **On an arm64 host,** a version with no build for it isn't offered, and the newest one that has one is.
- **Other tags,** such as `latest` or `stable`, are tracked by digest: an update means the tag now points at a new build.

## An update is Held

**Held** means ddum will not deploy the update. The reason is shown next to it.

| Reason | What to do |
|---|---|
| `protected: update this component manually…` | The container is a ddum agent or has `ddum.protected=true`. Update it yourself, then run `/update check`. |
| `host pending approval` | Approve the host. |
| `agent offline` | See [a host is offline](#a-host-is-offline). |
| `manual update required (standalone_manual)` | A container started with `docker run`. Recreate it yourself with the new image. |
| `manual update required (compose_files_unavailable: … not under COMPOSE_ALLOWED_ROOTS)` | Add the stack's directory to `COMPOSE_ALLOWED_ROOTS`. |
| `manual update required (compose_files_unavailable: … mount it at the same path …)` | Mount the directory into the agent **at the same path** as on the host, read-write. See [mounts](operator-guide.md#mounts-and-allowed-roots). |
| `manual update required (replicas_unsupported: service …)` | A service runs more than one replica. Not supported yet. |
| `manual update required (templated_image)`, `(tag_from_default)`, `(shared_anchor)`, `(shared_variable)` | ddum cannot change this tag safely. The image name is built from variables, comes from a `${VAR:-default}`, or the tag is shared with other services. Write the tag as a literal in `image:`, or give each service its own `.env` variable. Digest-only updates of the same tag still work. |
| `manual update required (git_managed)` | A Portainer Git stack: change the tag in the repository. Use the release link in the message. |
| `manual update required (not a stack of the configured Portainer environment)` | The stack lives in Portainer but in another environment, or the Portainer provider is not set up. See [Portainer](operator-guide.md#portainer). |
| `policy conflict: …` | The members of a group have different modes. Approving the whole group by hand still works. To deploy automatically, give the group one policy with `/policy set scope:group`. |
| `group_spans_hosts` | A group's members are on more than one host. Groups must stay on one host; remove the extra members with `/group remove`. |

## A deploy failed

The result message, **❌ Deploy failed · <stack>**, shows a code under **Why**. `/history show` adds the steps and the tail of the command output.

| Code | Meaning | What to do |
|---|---|---|
| `pull_failed` | The new image could not be pulled. Nothing was changed. | Check the registry and credentials on that host (`docker pull` there). |
| `digest_mismatch` | The pulled image was not the approved digest. Nothing was recreated. | The tag moved during the deploy. Run `/update check` and approve again. |
| `deploy_failed` | `docker compose up`, or the Portainer redeploy, failed. | Read the output tail. Use **Roll back** if offered. |
| `verify_failed` | The container did not end up running the approved image, or was not healthy and stable within the verify timeout. | Check the container's logs. Use **Roll back**. To give slow starters more time, raise `verify_timeout_seconds` with `/policy set`. |
| `unsafe` | The agent refused the command under its safety rules. | Usually the stack changed between approval and deploy, for example a different image repository or a removed service. Run `/update check`. If it persists, the log shows which rule. |
| `unsupported`, `ineligible` | The agent cannot do this on that stack, and changed nothing. | The reason says why. See [an update is Held](#an-update-is-held), or [recreating a service "would change it"](#recreating-a-service-would-change-it). |
| `interrupted` | The agent restarted mid-deploy, was replaced or removed, or lost its data volume and no longer knows the operation (reported after 10 minutes). | Check the stack, then roll back or run `/update check`. |

With `auto_rollback` on, which is the default for automatic updates, a failed deploy is rolled back by itself: **↩️ Rollback queued**.

A failed automatic deploy is not tried again. The update then waits for your approval, whatever the policy says, and its card says why. A newer release deploys automatically as usual.

If `docker compose up` fails and no container changed, ddum puts the Compose file back as it was.

**"The running image is not in this repository"** on an update card means the repository doesn't hold what the container runs. If that's a local build, approving replaces it with whatever the registry has under that name. If the registry just removed the old image, approving is fine.

## Recreating a service "would change it"

ddum refused a Compose deploy, because Compose inside the agent would recreate the service differently from how it runs now, and that isn't what you approved. Nothing was changed. The reason names what would differ:

- **`/data would be mounted from /root/data instead of /home/me/data`:** the stack uses `~` or `${HOME}`. Add `STACK_ENV_HOME: ${HOME}` to the agent and recreate it.
- **`these variables would change: PUID, TZ`:** they come from your shell, not from the stack's files. Put them in the stack's `.env`, or add `STACK_ENV_PUID: "1000"` to the agent. Only the names are shown, because values can be secrets.
- **The Compose file was edited** without `docker compose up`: run that yourself first, then `/update check`.

See [variables your stacks need](operator-guide.md#variables-your-stacks-need). **"docker compose cannot load this stack from inside the agent"** usually means a file the stack uses isn't mounted into the agent at the same path. The reason names it.

## A rollback failed and the stack is held

**❌ Rollback failed · <stack>** puts the stack on hold. ddum won't touch it, and won't deploy automatically, until you've looked. The stack may be half on the old images and half on the new ones.

If the reason says it was refused before anything changed, such as the Compose file being gone or naming a tag you set yourself, there's no hold. Fix the cause and try again.

A rollback only puts the image tag back. Anything else you changed in the files since the deploy stays.

1. Look at `/history show` and the containers on the host.
2. Get the stack into a working state by hand, for example by editing the tag and running `docker compose up -d`.
3. Click **Clear hold** on the result message, then run `/update check`.

## A bot update failed

**↩️ Bot update failed, previous version restored** means the new bot did not become healthy. The agent on its host put the previous image back. Check the logs of the failed version with `docker compose logs ddum-bot`; the new container's output comes first. Once the cause is fixed, run `/update check` to have the update offered again.

## A stack went missing or appeared twice

ddum identifies Compose stacks by project name. If you renamed the directory or the `name:` of a project, or moved it to another host, the old stack goes missing and a new one appears with fresh policies. ddum suggests a merge when the services match (**🔀 Was <old> renamed?**). Otherwise use `/stack merge source:<new> into:<old>`. See [renamed and moved stacks](operator-guide.md#renamed-and-moved-stacks).

Missing stacks are hidden from lists and notifications, never deleted. Their pending updates are closed after `STACK_MISSING_GRACE_HOURS` (24).

## Changed outside ddum

**🔀 <container> changed outside ddum** means a container now runs a different image than the one ddum last deployed to it, for example after a manual `docker compose pull && up`. ddum accepts the new state; this is only a heads-up, posted once. If it was not you, check who else manages that host, such as Watchtower or a Portainer webhook.

Whatever you do by hand, ddum follows it:

- **A pending update you got past** disappears from its card within one poll. If the update is still newer than what now runs, the card shows the new starting point.
- **A digest update of `latest` or a similar tag** stays on the card until the next check, because only the registry can say which digest is newer, and **Approve** refuses it until then. Run `/update check` to settle it now.
- **Roll back** is refused once a container runs something else, because it would undo your change.
- **"The Compose file names X, but the container runs Y"** on a failed deploy means the file was edited without `docker compose up`. Run that, or undo the edit, then `/update check`.

## Database problems

- **Keep `DATABASE_PATH` on a local disk.** SQLite locking does not work reliably on network shares such as SMB or NFS, so the database can be corrupted there. Use a Docker volume or a local directory.
- **"database is locked"** under normal use usually means something else holds the file open, such as a backup tool copying it. Back up with `ddum-bot backup` instead; see [backups](operator-guide.md#backups).
- **Restore a backup** as described in the [operator guide](operator-guide.md#backups). The restored bot knows nothing that happened after the backup, so run `/update check` afterwards, and check `/history list` against what you remember deploying.

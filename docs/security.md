# ddum security guide

What ddum protects, how it does so, what it can't protect against, and how to run it safely. To report a problem, see [SECURITY.md](../SECURITY.md).

## Contents

- [The short version](#the-short-version)
- [What each part can do](#what-each-part-can-do)
- [What stops a compromised bot](#what-stops-a-compromised-bot)
- [Keys](#keys)
- [Network](#network)
- [Discord](#discord)
- [Secrets](#secrets)
- [Hardening checklist](#hardening-checklist)

## The short version

- **The agent is root on its host,** because it has the Docker socket. Treat a host's agent like root access to that host.
- **The bot decides what should run, and an agent decides whether that's safe.** Even a compromised bot can't run a command on a host, or point a container at another repository.
- **Each agent has its own key.** One compromised host can't pose as another.
- **Only people you list can do anything,** and there are no finer roles among them.
- **Traffic between agents and the bot is signed, not encrypted.** Keep it on a private network.

## What each part can do

| Part | Holds | Can |
|---|---|---|
| Agent | Docker socket, read-write access to your stack folders, Portainer token, its own private key | Anything on its own host. That's what the socket means. |
| Bot | Discord token, registry logins, the bot's private key, a full inventory of your hosts | Ask agents to move containers to other published versions of their images. Post to one Discord channel. |
| Authorized Discord users | Nothing locally | Approve, schedule, snooze and roll back on every host, change policies, and approve new hosts. |

## What stops a compromised bot

An agent checks every command against rules the bot can't change, and refuses anything else. It:

- only touches stacks and containers it found itself on its own host;
- never changes an image's repository, only its tag and digest;
- only accepts images pinned by digest, and checks the pulled image against that digest before recreating anything;
- only edits files that resolve, after following symlinks, under `COMPOSE_ALLOWED_ROOTS`;
- never runs a command built from a string the bot sent. Commands are fixed argument lists with no shell;
- never touches a container labelled `ddum.protected=true`, or an agent;
- refuses a deploy if Compose would recreate the container differently from how it runs now, apart from its image.

**What's left:** a compromised bot can still ask for any published version of an image a container already uses, so it could move something to an old release with known holes. It can also stop reporting, or report falsely in Discord.

**Images are trusted as far as their repository is.** ddum deploys what a repository publishes. If a publisher's account is compromised, an approved update can bring that in, like any other way of updating. Automatic policies make that faster, which is why they're opt-in.

## Keys

- **The shared key (`AGENT_SHARED_KEY`) only introduces a new agent.** A new agent proves it holds the key, and then appears in Discord for you to approve. It still gets nothing until someone approves it.
- **Each agent has its own Ed25519 key,** made on first start and kept in `DATA_DIR/agent_key`. The bot pins the public key when the agent first connects, and from then on the agent's requests stand on its own signature. Someone holding the shared key can't pose as it.
- **The bot has a key too,** kept in its database. Each agent pins it on first contact and refuses answers signed with anything else.
- **If an agent's key changes,** say because its data volume was lost, its requests are refused and Discord asks whether to accept the new key. Check the fingerprint against the agent's log before accepting: someone else holding the shared key could be behind it.
- **Requests** carry a timestamp and a nonce, so they can't be replayed. **Responses** are bound to the request they answer, and cover their status and body.

**Limits:**
- Someone who holds the shared key and can intercept traffic could answer a brand-new agent's very first contact and get it to pin the wrong bot key. To close that off, set `BOT_PUBLIC_KEY` on the agent to the bot's key. `/system status` shows its fingerprint.
- Changing the shared key only affects new agents. Existing ones don't need it any more.

## Network

**The agent-to-bot protocol is plain HTTP.** It's signed, so it can't be forged or altered, but anyone on the path can read your inventory: host names, stacks, images and versions. Commands are readable too, though not forgeable.

- **One host:** publish no port. The agent reaches the bot over the Compose network.
- **Several hosts on one LAN:** bind the published port to the bot host's LAN address, not to all interfaces.
- **Hosts on different networks:** use a private overlay such as WireGuard or Tailscale.
- **Never expose the port to the internet.**

A reverse proxy that terminates TLS can sit in front of the bot, as long as it doesn't change the request path. `BOT_URL` accepts `https://`.

Agents never accept incoming connections, so they need no open port.

## Discord

- Only users in `AUTHORIZED_DISCORD_IDS` can do anything, including reading. That's checked for every command, button and menu, and only for your server.
- **The notification channel shows your inventory, so keep it private.** ddum warns once if `@everyone` can see it.
- Hiding the slash commands from other members is cosmetic. The allowlist is the real check.
- Secrets are never accepted through Discord.
- Anyone who gets into an authorized person's Discord account gets what that person can do, so use two-factor authentication.

## Secrets

- Tokens and keys go in environment variables or `_FILE` secrets, and aren't logged.
- **The agent keeps your secrets away from stacks.** `docker compose` runs with only the settings it needs, so a stack can't read the agent's token through a variable.
- **Snapshots hold no copies of your files,** only metadata and pinned images.
- **Failure output is masked:** values from env files and Portainer variables are replaced with `***` before leaving the agent. A secret written straight into a Compose file can't be told from the rest of the file, so an error that quotes it can reach Discord. Keep secrets in env files.
- **The bot's database is private to the bot,** because it holds the bot's private key and your inventory. Treat backups the same way.
- **The agent can read every file under its allowed roots,** including `.env` files, and it's root on the host anyway.

## Hardening checklist

- [ ] The notification channel is private.
- [ ] `AUTHORIZED_DISCORD_IDS` lists only people who should deploy, and they use two-factor authentication.
- [ ] The bot's port is unpublished, or bound to a private address.
- [ ] **Public Bot** is off for the Discord application.
- [ ] `.env` files are `chmod 600` and not in Git.
- [ ] `COMPOSE_ALLOWED_ROOTS` lists only the folders ddum should edit.
- [ ] `PORTAINER_CA_FILE` is used, not `PORTAINER_INSECURE_TLS`. The Portainer token belongs to a user that can only reach the stacks ddum should update.
- [ ] Databases, proxies and anything you update by hand are labelled `ddum.protected=true`.
- [ ] Automatic policies are only on stacks you're happy to see updated unattended.
- [ ] You back up `ddum-bot-data`, and keep the copies private.

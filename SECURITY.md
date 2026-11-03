# Security policy

ddum's agents hold the Docker socket on your hosts, which is root access, so security problems matter. Thanks for taking the time to report one.

## Reporting a problem

**Please don't open a public issue.** Use GitHub's private reporting instead: the repository's **Security** tab, then **Report a vulnerability**. Include what you found, how to reproduce it, and what version you tried.

ddum is maintained by one person in their spare time, so there's no promise of a deadline, but you should hear back within a week or two. Reports are credited in the release notes unless you'd rather they weren't.

## Which versions get fixes

ddum is in public beta. Fixes go into the latest release only, so please update before reporting if you can.

## What counts

Worth reporting:

- Anything that lets someone run a command on a host through ddum, or change a file outside `COMPOSE_ALLOWED_ROOTS`.
- A way to approve or deploy anything without being in `AUTHORIZED_DISCORD_IDS`.
- A way for one host's agent, or someone holding the shared key, to act as another host that already has a key, or to forge the bot's answers to it.
- A deploy or rollback that changes something other than an image tag in your files, or that reports success when it failed.
- A secret from an env file reaching the logs, the database or Discord.

Known and documented, so not a vulnerability in itself:

- An **agent has the Docker socket**, so whoever controls a host's agent controls that host.
- Anyone in `AUTHORIZED_DISCORD_IDS` can deploy and roll back on every host. There are no finer roles.
- **The agent-to-bot traffic is plain HTTP.** It's signed, but anyone on the path can read your inventory. Use a private network ([security guide](docs/security.md#network)).
- ddum isn't meant to face the internet directly.
- A secret written straight into a Compose file, rather than an env file, can appear in an error that quotes it.

## Using it safely

The [security guide](docs/security.md) covers what ddum protects and how, with a hardening checklist.

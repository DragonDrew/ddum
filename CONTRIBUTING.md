# Contributing

Bug reports and pull requests are welcome. ddum is a small project maintained in spare time, so a reply can take a little while. For anything large, open an issue first, so nobody spends time on something that doesn't fit.

**Security problems go through [private reporting](SECURITY.md)**, not a public issue.

## Reporting a bug

Include the ddum version (`/system status`), how the stack is deployed (Compose or Portainer), and the agent's log around the problem. Check logs and cards for host names or anything else you'd rather not post.

## Making a change

You need Go (the version in `go.mod`). No C toolchain is needed, except for the race detector.

```sh
make check      # vet, gofmt, the tests that need no Docker, and the Linux builds
make test-race  # the same tests with the race detector
make e2e        # real deploys and rollbacks against real Docker, on a Linux host
```

On Windows without `make`, run `go vet ./...` and `go test ./...`.

- Keep agent safety rules intact: an agent refuses anything but a digest-pinned update of an image it found itself, and runs no command built from a string the bot sent.
- A database migration only adds things, because a failed bot update puts the previous bot back on the migrated database.
- Add a test with your change. A change to deploys, rollbacks or the protocol should also pass `make e2e`.
- User-facing behaviour goes in the [operator guide](docs/operator-guide.md) or [troubleshooting](docs/troubleshooting.md), and in [CHANGELOG.md](CHANGELOG.md).

By contributing, you agree that your work is licensed under the [Apache License 2.0](LICENSE).

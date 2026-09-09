# mux — agent notes

mux is a library: one interface over tmux, herdr and cmux for a window
to type into, a state to read back, a way to focus it and a way to
notify (README.md). owl and spaces import it; a change here reaches
users only through their releases.

A change is done when it is committed in coherent pieces, pushed with
CI green, documented, tagged when a consumer needs it, and picked up by
that consumer. Do all of it and say which steps you did.

## Build and test

```sh
make test      # go test -race ./... — the tmux tests run a private server
make lint      # gofmt, go vet
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
```

Check exit codes, not output. The fakes in muxtest/ stand in for
herdr and cmux (a test binary serves the CLI under the fake's name);
every driver verb the fakes answer is one they print in the real
tool's shape — copy the real output into the fake when a verb is added.
Read the live tools only read-only (`cmux list-status`, `tmux
list-windows`); never start tmux servers, herdr servers or the cmux
app from the agent's shell — an app launched from a Claude session
inherits its markers, and every agent in it saves no transcript.

## Commit

Conventional commits, lower-case subject, a body that says why; the
scope is the driver (`cmux`, `herdr`, `tmux`) or `test`. Smallest
coherent commits — the fake first, the driver on top, the docs beside.
No Co-Authored-By trailers.

## Document

README.md has the table of what each driver does per verb; a
behaviour change changes its row. The pill contract with claude-status
— key `claude`, values working/blocked/done/idle, read with `cmux
list-status --workspace <id>` — is shared: changing it changes
claude-status too.

## Ship

A library ships as a tag; there is no release workflow.

```sh
git tag -a v0.5.1 -m "mux 0.5.1: <the batch, one line>"
git push origin v0.5.1
```

Then the consumers: in owl and spaces, `GOPROXY=direct go get
github.com/stefanahman/mux@v0.5.1 && go mod tidy`, one commit `build:
mux v0.5.1 — <why>`, and their own ship steps (their CLAUDE.md).
Patch for fixes, minor for a new verb on the Driver interface.

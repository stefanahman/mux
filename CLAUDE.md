# mux — agent notes

mux is a library: one interface over tmux, herdr and cmux for a window
to type into, a state to read back, a way to focus it and a way to
notify (README.md). owl and spaces import it; a change here reaches
users only through their releases.

A change is done when it is committed in coherent pieces, pushed with
CI green, documented, tried in a consumer, tagged when that consumer
needs it, and picked up there. Do all of it and say which steps you
did.

## Build and test

```sh
make test      # go test -race ./... — the tmux tests run a private server
make lint      # gofmt, go vet
go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...   # the version CI pins
```

Check exit codes, not output. Pin staticcheck to the version in
`.github/workflows/ci.yml` rather than `@latest`: a newer release
flags what CI accepts, an older one misses what CI rejects, and either
way the two disagree.

`make test && make lint` green does not predict CI. Three jobs run
there, and two of them do something this machine's `make` does not:

| job | runner | what it adds |
|---|---|---|
| `go` | ubuntu | `make lint`, then `make test` on the Go version in go.mod |
| `macos` | macos | `make test` against Homebrew's tmux — the real driver, on the platform every consumer runs |
| `analysis` | ubuntu, Go stable | `staticcheck@2026.2.1` and `govulncheck@v1.7.0` |

`gh run list --workflow ci --branch main --limit 1` after a push.

## The test doubles

`muxtest/` holds what a driver test runs against:

- `muxtest/cmux.go` and `muxtest/herdr.go` are fakes for tools that
  cannot run here — a test binary serves the CLI under the fake's
  name. Every verb they answer is one the real tool prints in that
  shape; when a driver learns a verb, copy the real output into the
  fake rather than inventing a shape.
- `muxtest/tmux.go` is not a fake. `StartTmux` starts a real tmux on a
  private socket, with `TMUX`, `HERDR_ENV` and `CMUX_WORKSPACE_ID`
  cleared so a test never adopts the multiplexer the agent runs in.
  It is exported and consumers import it for their own tmux tests, so
  changing its behaviour changes theirs.

Read the live tools read-only (`cmux list-status`, `tmux
list-windows`); never start tmux servers, herdr servers or the cmux
app from the agent's shell — an app launched from a Claude session
inherits its markers, and every agent in it saves no transcript.

**`go.mod` has no dependencies, and that is the point.** mux is
imported by every Go tool in this family, so a dependency here lands
in all of them. The standard library and `os/exec` around the
multiplexers' own CLIs are the budget. Adding one is a decision to
raise with Stefan, not a detail to slip into a commit.

## Commit

Conventional commits, lower-case subject, a body that says why. Most
of this history carries no scope and one commit carries `(cmux)`, so
there is no established list: scope by what moved — the driver
(`cmux`, `herdr`, `tmux`) when one driver changes, no scope when the
interface or the package does. Smallest coherent commits: the fake
first, the driver on top, the docs beside. No Co-Authored-By trailers.

## Document

README.md has the table of what each driver does per verb; a
behaviour change changes its row. The pill contract with claude-status
— key `claude`, values working/blocked/done/idle, read with `cmux
list-status --workspace <id>` — is shared: changing it changes
claude-status too.

## Try it in a consumer, then ship

A library's own tests prove the library. What breaks is the consumer,
and finding that after the tag costs a second tag. Before tagging, run
owl's suite against this working tree:

```sh
cd ~/Development/owl
go mod edit -replace github.com/stefanahman/mux=../mux
make test
go mod edit -dropreplace github.com/stefanahman/mux   # leave no replace behind
```

Same for spaces when the change touches what it uses. The replace is a
local experiment; never commit a `go.mod` carrying one.

Then the tag. A library ships as a tag: there is no release workflow
here, and `gh run list --workflow release` says as much.

```sh
git tag -a v0.5.1 -m "mux 0.5.1: <the batch, one line>"
git push origin v0.5.1
```

Then the consumers: in owl and spaces, `GOPROXY=direct go get
github.com/stefanahman/mux@v0.5.1 && go mod tidy`, one commit `build:
mux v0.5.1 — <why>`, and their own ship steps (their CLAUDE.md).
Patch for fixes, minor for a new verb on the Driver interface.

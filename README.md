# mux

One Go interface over tmux, herdr and cmux, for coding agents.

[![ci](https://github.com/stefanahman/mux/actions/workflows/ci.yml/badge.svg)](https://github.com/stefanahman/mux/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/stefanahman/mux.svg)](https://pkg.go.dev/github.com/stefanahman/mux)
[![license](https://img.shields.io/github/license/stefanahman/mux)](LICENSE)

mux drives the terminal multiplexers that hold coding agents. One
interface for what they share: a workspace per task, panes inside it, a
shell to run a command in, an agent to hand a prompt to, and the agent's
state. Each multiplexer's quirks stay in its own driver.

## Install

```sh
go get github.com/stefanahman/mux
```

## Quick start

```go
import "github.com/stefanahman/mux"

d := mux.Detect(mux.Tmux{SessionName: "work"}, mux.NewHerdr(""), mux.Cmux{})
_ = d.Prepare("/repo")
ws, _ := d.Create("pr-42", "/repo/.worktrees/pr-42")
pane, _ := d.AgentPane(ws)
_ = d.Run(ws, pane, `claude "review PR 42"`)
states, _ := d.States() // states["pr-42"]: working, blocked, done or idle
```

`Detect` picks the multiplexer this process runs in, else tmux.

## Docs

- [The model](docs/model.md): workspaces, panes and tabs in each multiplexer
- [Capabilities](docs/capabilities.md): grouping, watching and moving workspaces
- [Drivers](docs/drivers.md): what was verified on each tool
- [Testing](docs/testing.md): the fakes in `muxtest`, and a private tmux server

Used by [owl](https://github.com/stefanahman/owl) and
[spaces](https://github.com/stefanahman/spaces).

See also: [owl](https://github.com/stefanahman/owl) ·
[spaces](https://github.com/stefanahman/spaces) ·
[claude-status](https://github.com/stefanahman/claude-status) ·
[mindoro](https://github.com/stefanahman/mindoro) ·
[mcp-defer](https://github.com/stefanahman/mcp-defer) ·
[eden](https://github.com/stefanahman/eden)

## License

MIT

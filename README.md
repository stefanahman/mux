# mux

Go drivers for the terminal multiplexers that hold coding agents:
**tmux**, **herdr** and **cmux**. One interface for what they share —
a workspace per task, panes inside it, a shell to type a command line
into, an agent to hand a prompt to, and what the multiplexer knows
about that agent — with each multiplexer's quirks kept in its own
file, next to what was verified on the real thing.

[pr-owl](https://github.com/stefanahman/pr-owl) runs its reviews on
it; [spaces](https://github.com/stefanahman/spaces) builds work
contexts inside herdr and cmux with it.

```go
import "github.com/stefanahman/mux"

d := mux.Detect(mux.Tmux{SessionName: "reviews"}, mux.NewHerdr(""), mux.Cmux{})
ws, _ := d.Create("pr-42-fix-crash", "/repo/.worktrees/pr-42")
pane, _ := d.AgentPane(ws)
_ = d.Run(ws, pane, `claude "/pr-review:pr-review 42"`)   // a shell line, Enter included
states, _ := d.States()                                      // states["pr-42-fix-crash"]: working, blocked, done, idle, ""
if d.AtShell(ws, pane) { /* the agent has exited */ }
_ = d.Prompt(ws, pane, "look at the new comments")           // the multiplexer's own way where it has one
```

`Detect` picks herdr or cmux when the process runs inside one, tmux
otherwise. `ByKind("cmux")` names one.

## The model

| | tmux | herdr | cmux |
|---|---|---|---|
| workspace | a window of the driver's session | a workspace | a workspace |
| pane | a pane | a pane | a terminal surface |
| tab (`AddTab`) | — (`ErrUnsupported`) | a tab | a surface in the first pane |
| `Layout` | the window as one tab | tabs with their panes | the first pane's surfaces as tabs; the other panes, cmux's workspace-wide splits, listed under the first |
| the agent's state | `@claude-state`, written by [tmux-claude-status](https://github.com/stefanahman/tmux-claude-status) | herdr's own detection, from the screen | cmux's Claude Code hooks, through the wrapper it puts on the shell's PATH |
| `Prompt` | keystrokes | `agent.prompt`, which refuses while the agent is blocked; keystrokes for an agent herdr has not detected | keystrokes |
| `Processes` | the pane's current command | the pane's foreground processes | what `top` files under the pane's first surface — cmux does not say which tab |
| `Inside` | `TMUX` | `HERDR_ENV=1` | `CMUX_WORKSPACE_ID` |
| `Focus` | `switch-client` | nothing: every client follows `Select` | `focus-window`, from outside cmux |
| `Notify` | the status line, eight seconds | a herdr notification | a cmux notification |
| transport | the `tmux` command | the session's socket, newline JSON | the `cmux` command |

`States` speaks four words: `working`, `blocked` (a permission or a
question waits), `done` (finished, not yet looked at), `idle`; `""` is
unknown. `Run` types a shell line; `Prompt` addresses the agent.

## What each driver verified

**tmux.** Windows are matched exactly (`=session:=window`): without the
prefix tmux matches prefixes, and `pr-1` finds `pr-12`. A created
window has `automatic-rename off`, or tmux renames it after the
process in it. State is whatever `@claude-state` holds; without the
plugin every state is unknown and everything else works.

**herdr** (0.9). One request per socket connection. `workspace.create`
answers with the root pane. `pane.split` takes `target_pane_id`; a
`pane_id` is ignored and the focused pane split instead. `agent.prompt`
answers `agent_blocked` while a dialog is up and `agent_not_found` for
an agent it has not detected, in which case the text is typed. The
session's name is `HERDR_SESSION` in a pane's environment (seen, not
documented), else read from a `…/sessions/<name>/herdr.sock` path.

**cmux** (0.64.22). The socket admits only processes started inside
cmux unless cmux was launched with `CMUX_SOCKET_MODE=allowAll`;
`Ping` says so. Handles are UUIDs — refs like `workspace:2` renumber.
Its terminals carry `CMUX_WORKSPACE_ID` and `CMUX_TAB_ID` but not the
`CMUX_SURFACE_ID` its Claude Code wrapper checks before injecting the
hooks, so `Run` types `CMUX_SURFACE_ID=<id> …` when this process lacks
the variable; a cmux that sets it gets the line as it is. Hook records
come from `cmux sessions --agent claude`: `running`, `needsInput`,
`idle`, kept after the agent exits (`stored_pid_exists` tells). `done`
is idle with cmux's notification about the turn unread; `Select` marks
them read. `top` files a pane's processes under its first surface, and
lists nothing for an idle shell.

## Testing against it

`muxtest` has doubles for all three: `NewFakeHerdr` serves herdr's
wire shapes on a socket, `InstallFakeCmux` puts a fake `cmux` on PATH
(the test binary itself — its `TestMain` hands the name to
`FakeCmuxMain`), `StartTmux` runs a private tmux server. mux's own
tests are the example.

```sh
make test   # -race; tmux on PATH runs the tmux tests, otherwise they skip
make lint
```

## License

MIT

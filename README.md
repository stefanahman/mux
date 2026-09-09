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
_ = d.Prepare("/repo")                                       // tmux: the session; herdr, cmux: a ping
ws, _ := d.Create("pr-42-fix-crash", "/repo/.worktrees/pr-42")
pane, _ := d.AgentPane(ws)
_ = d.Run(ws, pane, `claude "/pr-review:pr-review 42"`)   // a shell line, Enter included
states, _ := d.States()                                      // states["pr-42-fix-crash"]: working, blocked, done, idle, ""
if d.AtShell(ws, pane) { /* the agent has exited */ }
_ = d.Prompt(ws, pane, "look at the new comments")           // the multiplexer's own way where it has one
```

`Detect` picks herdr or cmux when the process runs inside one, tmux
otherwise. `ByKind("cmux")` names one. `Tmux`'s zero value is pr-owl's
review session, `pr-reviews` with a `scratch` keepalive window; set
`SessionName` and `Keepalive` for anything else.

## The model

| | tmux | herdr | cmux |
|---|---|---|---|
| workspace | a window of the driver's session | a workspace | a workspace |
| pane | a pane | a pane | a terminal surface |
| tab (`AddTab`) | — (`ErrUnsupported`) | a tab | a surface in the first pane |
| `Layout` | the window as one tab | tabs with their panes | the first pane's surfaces as tabs; the other panes, cmux's workspace-wide splits, listed under the first |
| the agent's state | `@claude-state`, written by [claude-status](https://github.com/stefanahman/claude-status) | herdr's own detection, from the screen | claude-status's pill when the plugin runs there, else cmux's Claude Code hooks (whose needsInput also covers the idle reminder) |
| `Prompt` | keystrokes | `agent.prompt`, which refuses while the agent is blocked; keystrokes for an agent herdr has not detected | keystrokes |
| `Processes` | the pane's current command | the pane's foreground processes | `ps` on the surface's tty where cmux knows it; else what the tab's title names |
| `Inside` | `TMUX` | `HERDR_ENV=1` | `CMUX_WORKSPACE_ID` |
| `Focus` | `switch-client` | nothing: every client follows `Select` | `focus-window`, from outside cmux |
| `Notify` | the status line, eight seconds; nothing outside tmux | a herdr notification | a cmux notification |
| grouping (`mux.Group`) | — the session already is the container | — no grouping in its API | a collapsible sidebar group, anchored on its first member |
| transport | the `tmux` command | the session's socket, newline JSON | the `cmux` command |

`Group` is a capability, not a Driver verb: `mux.Group(d, name, ws,
style)` puts a workspace in a named group where the multiplexer has
them, and does nothing where it does not, so a caller never branches on
`Kind`. Only `Cmux` implements `Grouper`. The group is created on the
first workspace that needs it and anchored there, found by name
afterwards, and it disappears with its last member. `GroupStyle`'s
colour and SF Symbol are applied on creation and are best effort: a
style the multiplexer refuses leaves the workspace grouped and returns
no error.

`States` speaks four words: `working`, `blocked` (a permission or a
question waits), `done` (finished, not yet looked at), `idle`; `""` is
unknown. Under tmux and cmux the state is Claude Code's, from its
hooks; herdr detects several agents from the screen. `Run` types a
shell line; `Prompt` addresses the agent.

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
The hooks are cmux's Claude Code integration, on through
`automation.claudeCodeIntegration: true` in `~/.config/cmux/cmux.json`.
Its terminals carry `CMUX_SURFACE_ID`, which its Claude Code wrapper
checks before injecting the hooks. The state is read from two places.
First claude-status's sidebar pill, `claude=working|blocked|done|idle`
(`cmux list-status --workspace <id>`, one call per workspace, run side
by side: the CLI's start-up is the cost, the same for one as for
fifteen), which its hooks write from the events that mean those words.
Without the pill, the hook records from `cmux sessions --agent claude`:
`running`, `needsInput`, `idle`, kept after the agent exits
(`stored_pid_exists` tells). Those are the fallback because cmux sets
`needsInput` on Claude Code's idle reminder too — the notification
sent 60 seconds after a finished turn — so a session at its prompt
reads as blocked there once a minute has passed. Either way `done` is
done while cmux's notification about the turn is unread; `Seen` marks
them read (owl calls it when the user arrives), and a visit in cmux
does the same. cmux knows the tty only of the surface a workspace was
created with; a tab or split made through its API has none, and its
`top` samples miss idle programs. So `Processes` runs `ps` on the tty
where there is one — exact — and otherwise reads the tab's title, which
cmux's shell integration keeps: the running program's line, or the
directory at a prompt (`Terminal` before the first one). Reads come
from one snapshot per driver (`NewCmux`): the list, the tree, the
pills and `ps`, taken once and dropped when the driver changes
something.

## A tainted server or app

A multiplexer's terminals inherit the environment of its server or
app, and an app launched from a shell gets that shell's environment
(macOS's `open` hands it over). Two things in it break agents quietly:
`TMUX` makes cmux's shell integration hand `CMUX_SURFACE_ID` to tmux
before every command, so its Claude Code hooks never engage; Claude
Code's own session markers (`CLAUDECODE`, `CLAUDE_CODE_CHILD_SESSION`)
make every agent a child session that saves no transcript. `Ping`
reads the tmux server's global environment and the environment of the
process holding the herdr or cmux socket (`lsof` on macOS, `ss` on
Linux), and fails naming the marker and the fix: relaunch from a
hotkey or a plain shell (`errors.Is(err, mux.ErrTainted)` tells that
failure from an unreachable multiplexer). Launchers pass `CleanEnv(os.Environ())` to
what they start, so a launch from any shell comes out clean, and
`Tmux.Prepare` starts a server without the markers itself.

## Testing against it

`muxtest` has doubles for all three: `NewFakeHerdr` serves herdr's
wire shapes on a socket, `InstallFakeCmux` puts a fake `cmux` on PATH
(the test binary itself — its `TestMain` hands the name to
`FakeCmuxMain`), `StartTmux` runs a private tmux server. All three
clear the variables herdr and cmux put in a terminal's environment,
so a test run from inside one still lands on its double — `Detect`
would otherwise reach the real thing. mux's own tests are the example.

```sh
make test   # -race; tmux on PATH runs the tmux tests, otherwise they skip
make lint
```

## License

MIT

# The model

`Detect` picks herdr or cmux when the process runs inside one, tmux
otherwise. `ByKind("cmux")` names one. `Tmux`'s zero value is owl's
review session, `reviews` with a `scratch` keepalive window; set
`SessionName` and `Keepalive` for anything else.

| | tmux | herdr | cmux |
|---|---|---|---|
| workspace | a window of the driver's session | a workspace | a workspace |
| pane | a pane | a pane | a terminal surface |
| tab (`AddTab`) | — (`ErrUnsupported`) | a tab | a surface in the first pane |
| `Layout` | the window as one tab | tabs with their panes | the first pane's surfaces as tabs; the other panes, cmux's workspace-wide splits, listed under the first |
| the agent's state | `@claude-state`, written by [claude-status](https://github.com/stefanahman/claude-status) | herdr's own detection, from the screen | cmux's own Claude Code hooks; a `claude` sidebar pill still wins where something writes one |
| `Prompt` | keystrokes | `agent.prompt`, which refuses while the agent is blocked; keystrokes for an agent herdr has not detected | keystrokes |
| `Processes` | the pane's current command | the pane's foreground processes | `ps` on the surface's tty where cmux knows it; else what the tab's title names |
| `Inside` | `TMUX` | `HERDR_ENV=1` | `CMUX_WORKSPACE_ID` |
| `Focus` | `switch-client` | nothing: every client follows `Select` | `focus-window`, from outside cmux |
| `Notify` | the status line, eight seconds; nothing outside tmux | a herdr notification | a cmux notification |
| ending it from inside (`SelfClose`) | `exit`: a window goes with its last pane's process, `remain-on-exit` being off | `exit`: a workspace goes with its last pane's shell | `cmux workspace close`: the shell outlives the command by design, so the workspace has to be told |
| grouping (`mux.Group`) | — the session already is the container | — no grouping in its API | a collapsible sidebar group, anchored on its first member |
| watching (`mux.Watch`) | — nothing to listen to | — its stream is not read yet | `cmux events` over the socket: `States` then costs nothing |
| reordering (`mux.Move`) | — `move-window` exists, no driver for it yet | `workspace.move`; the index is where the workspace ends up | — `reorder-workspace` exists, no driver for it yet |
| transport | the `tmux` command | the session's socket, newline JSON | the `cmux` command, at `Socket` when the driver names one |

A named herdr session lives at `sessions/<name>/herdr.sock` beside the
default session's socket, in `$XDG_CONFIG_HOME/herdr` (else
`~/.config/herdr`) as herdr keeps them; `mux.NewHerdrSession(name)` is
its driver, and `Session()` reads the name back from that path.

`States` speaks four words: `working`, `blocked` (a permission or a
question waits), `done` (finished, not yet looked at), `idle`; `""` is
unknown. Under tmux and cmux the state is Claude Code's, from its
hooks; herdr detects several agents from the screen. `Run` types a
shell line; `Prompt` addresses the agent.

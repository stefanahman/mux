# What each driver verified

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
Normally the hook records from `cmux sessions --agent claude`:
`running`, `needsInput`, `idle`, kept after the agent exits
(`stored_pid_exists` tells). A `claude=working|blocked|done|idle`
sidebar pill wins over them where one exists (`cmux list-status
--workspace <id>`, one call per workspace, run side by side: the CLI's
start-up is the cost, the same for one as for fifteen).

claude-status wrote that pill until its 0.4.0, because cmux used to set
`needsInput` on Claude Code's idle reminder too — the notification sent
60 seconds after a finished turn — so a session at its prompt read as
blocked once a minute had passed. Upstream fixed that, claude-status
stopped writing, and the hook records are the answer again; the pill is
read still, for anyone who writes one. Either way `done` is
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

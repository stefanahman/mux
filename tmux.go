// tmux as the multiplexer: one session holds the workspaces as its
// windows, a keepalive window keeps it alive when there are none, and
// keystrokes go through send-keys. What the agent is doing comes from
// tmux-claude-status, which writes Claude Code's hook events to the
// @claude-state window option; without the plugin every state is
// unknown, and everything else still works.
package mux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ClaudeStateOption is the tmux window option tmux-claude-status
// writes Claude's state to — the contract between the tools.
const ClaudeStateOption = "@claude-state"

// Tmux drives a tmux session. The zero value uses the defaults below.
type Tmux struct {
	// SessionName holds the workspaces; default "pr-reviews".
	SessionName string
	// Keepalive is the window that keeps the session alive with no
	// workspaces open; default "scratch". It is never a workspace.
	Keepalive string
}

func (t Tmux) session() string {
	if t.SessionName == "" {
		return "pr-reviews"
	}
	return t.SessionName
}

func (t Tmux) keepalive() string {
	if t.Keepalive == "" {
		return "scratch"
	}
	return t.Keepalive
}

func tmux(args ...string) (string, error) {
	return runOut(exec.Command("tmux", args...))
}

// TmuxTarget builds an exact-match `-t` argument. Without the `=`
// prefix tmux falls back to prefix matching, so `pr-1` would resolve
// to `pr-12-foo` when `pr-1` itself doesn't exist.
func TmuxTarget(session, window string) string {
	if window == "" {
		return "=" + session
	}
	return "=" + session + ":=" + window
}

func (t Tmux) target(window string) string { return TmuxTarget(t.session(), window) }

func (Tmux) Kind() string { return "tmux" }

func (Tmux) Inside() bool { return os.Getenv("TMUX") != "" }

func (Tmux) ChildEnv() []string { return nil }

func (Tmux) Ping() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return errors.New("tmux: not on PATH")
	}
	return nil
}

// Prepare creates the session with its keepalive window when it
// doesn't exist.
func (t Tmux) Prepare(cwd string) error {
	if _, err := tmux("has-session", "-t", t.target("")); err == nil {
		return nil
	}
	if _, err := tmux("new-session", "-d", "-s", t.session(), "-n", t.keepalive(), "-c", cwd); err != nil {
		// Two children starting at once both saw no session; the
		// loser of the race finds the winner's.
		if _, again := tmux("has-session", "-t", t.target("")); again == nil {
			return nil
		}
		return err
	}
	return nil
}

// Workspaces are the session's windows but the keepalive one.
func (t Tmux) Workspaces() ([]Workspace, error) {
	out, err := tmux("list-windows", "-t", t.target(""), "-F", "#{window_id}\t#{window_name}\t#{pane_current_path}\t#{window_active}")
	if err != nil {
		return nil, err
	}
	var ws []Workspace
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 4 || f[1] == t.keepalive() {
			continue
		}
		ws = append(ws, Workspace{ID: f[0], Name: f[1], Cwd: f[2], Selected: f[3] == "1"})
	}
	return ws, nil
}

// Create adds a window without selecting it and freezes its name —
// otherwise tmux renames the window after the process in it, and the
// window stops matching the task.
func (t Tmux) Create(name, cwd string) (Workspace, error) {
	id, err := tmux("new-window", "-d", "-P", "-F", "#{window_id}", "-t", t.target(""), "-c", cwd, "-n", name)
	if err != nil {
		return Workspace{}, err
	}
	if _, err := tmux("set-option", "-w", "-t", id, "automatic-rename", "off"); err != nil {
		return Workspace{}, err
	}
	return Workspace{ID: id, Name: name, Cwd: cwd}, nil
}

func (t Tmux) Panes(ws Workspace) ([]Pane, error) {
	out, err := tmux("list-panes", "-t", ws.ID, "-F", "#{pane_id}")
	if err != nil {
		return nil, err
	}
	var panes []Pane
	for _, id := range strings.Split(out, "\n") {
		if id != "" {
			panes = append(panes, Pane{ID: id})
		}
	}
	return panes, nil
}

// Layout is the window as its single tab.
func (t Tmux) Layout(ws Workspace) ([]Tab, error) {
	panes, err := t.Panes(ws)
	if err != nil {
		return nil, err
	}
	return []Tab{{Name: ws.Name, Panes: panes}}, nil
}

// AgentPane is the window's first pane: tmux knows nothing of agents.
func (t Tmux) AgentPane(ws Workspace) (Pane, error) {
	panes, err := t.Panes(ws)
	if err != nil {
		return Pane{}, err
	}
	if len(panes) == 0 {
		return Pane{}, fmt.Errorf("tmux: window %s has no pane", ws.Name)
	}
	return panes[0], nil
}

func (Tmux) AddTab(Workspace, string, string) (Pane, error) { return Pane{}, ErrUnsupported }

func (t Tmux) Split(ws Workspace, pane Pane, dir Direction, cwd string) (Pane, error) {
	flag := "-h"
	if dir == Down {
		flag = "-v"
	}
	args := []string{"split-window", "-d", "-P", "-F", "#{pane_id}", flag, "-t", pane.ID}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	id, err := tmux(args...)
	if err != nil {
		return Pane{}, err
	}
	return Pane{ID: id}, nil
}

// Processes is the pane's current command: tmux tracks the foreground
// process of every pane itself.
func (Tmux) Processes(_ Workspace, pane Pane) ([]string, error) {
	out, err := tmux("display-message", "-p", "-t", pane.ID, "#{pane_current_command}")
	if err != nil {
		return nil, err
	}
	return []string{out}, nil
}

func (t Tmux) AtShell(ws Workspace, pane Pane) bool {
	ps, err := t.Processes(ws, pane)
	return err == nil && len(ps) == 1 && IsShell(ps[0])
}

// Run types the line as literal keystrokes, then Enter.
func (Tmux) Run(_ Workspace, pane Pane, line string) error {
	if _, err := tmux("send-keys", "-t", pane.ID, "-l", line); err != nil {
		return err
	}
	_, err := tmux("send-keys", "-t", pane.ID, "Enter")
	return err
}

// Prompt is keystrokes too: tmux has no notion of the agent.
func (t Tmux) Prompt(ws Workspace, pane Pane, text string) error { return t.Run(ws, pane, text) }

// States reads every window with the value of the state option. An
// absent value is "" (a fresh window, or no plugin).
func (t Tmux) States() (map[string]string, error) {
	out, err := tmux("list-windows", "-t", t.target(""), "-F", "#{window_name}\t#{"+ClaudeStateOption+"}")
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, state, _ := strings.Cut(line, "\t")
		if name == "" || name == t.keepalive() {
			continue
		}
		states[name] = state
	}
	return states, nil
}

func (Tmux) Select(ws Workspace) error {
	_, err := tmux("select-window", "-t", ws.ID)
	return err
}

// Focus switches the client this process belongs to over to the session.
func (t Tmux) Focus() error {
	_, err := tmux("switch-client", "-t", t.target(""))
	return err
}

func (Tmux) Close(ws Workspace) error {
	_, err := tmux("kill-window", "-t", ws.ID)
	return err
}

// Current is the window this process runs in, when that is one of the
// session's.
func (t Tmux) Current() (Workspace, bool) {
	if !t.Inside() {
		return Workspace{}, false
	}
	out, err := tmux("display-message", "-p", "#{session_name}\t#{window_id}\t#{window_name}\t#{pane_current_path}")
	f := strings.Split(out, "\t")
	if err != nil || len(f) < 4 || f[0] != t.session() || f[2] == t.keepalive() {
		return Workspace{}, false
	}
	return Workspace{ID: f[1], Name: f[2], Cwd: f[3], Selected: true}, true
}

func (t Tmux) Describe(ws Workspace) string { return t.session() + ":" + ws.Name }

func (t Tmux) AttachHint() string {
	if t.Inside() {
		return ""
	}
	return "tmux attach -t " + t.session()
}

// Notify puts the text on the status line of the client this process
// belongs to, for eight seconds; outside tmux there is no client.
func (t Tmux) Notify(title, text string) {
	if !t.Inside() {
		return
	}
	_, _ = tmux("display-message", "-d", "8000", title+": "+text)
}

func (t Tmux) Session() string { return t.session() }

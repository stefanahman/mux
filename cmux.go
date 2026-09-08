// cmux as the multiplexer: one cmux workspace per task, its terminal
// surface running the agent. Everything goes through the `cmux` CLI
// cmux ships, which finds the app's socket itself; the shapes below
// are the ones cmux 0.64.22 prints. State comes from cmux's own
// Claude Code hooks, which it injects through a wrapper when `claude`
// starts in one of its terminals.
package mux

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Cmux drives the cmux application through its CLI. Handles are
// cmux's UUIDs, which survive reordering; refs like workspace:2 do not.
type Cmux struct{}

// run runs one cmux command and returns trimmed stdout. CMUX_QUIET
// silences the notices cmux prints for its older verb names.
func (Cmux) run(args ...string) (string, error) {
	cmd := exec.Command("cmux", args...)
	cmd.Env = append(os.Environ(), "CMUX_QUIET=1")
	return runOut(cmd)
}

// runJSON runs a command with --json, ids and refs both, into v.
func (c Cmux) runJSON(v any, args ...string) error {
	out, err := c.run(append([]string{"--json", "--id-format", "both"}, args...)...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("cmux %s: %w", args[0], err)
	}
	return nil
}

func (Cmux) Kind() string { return "cmux" }

// Inside: cmux names the workspace in the environment of every
// terminal it runs.
func (Cmux) Inside() bool { return os.Getenv("CMUX_WORKSPACE_ID") != "" }

// ChildEnv is empty: a child inherits cmux's variables with the rest.
func (Cmux) ChildEnv() []string { return nil }

// Ping asks cmux; its socket admits only processes started inside
// cmux unless cmux was launched with CMUX_SOCKET_MODE=allowAll.
func (c Cmux) Ping() error {
	if _, err := c.run("ping"); err != nil {
		if c.Inside() {
			return err
		}
		return fmt.Errorf("%w (run from a cmux terminal, or start cmux with CMUX_SOCKET_MODE=allowAll)", err)
	}
	return nil
}

func (c Cmux) Prepare(string) error { return c.Ping() }

type cmuxWorkspace struct {
	ID          string `json:"id"`
	Ref         string `json:"ref"`
	Title       string `json:"title"`
	CustomTitle string `json:"custom_title"`
	HasCustom   bool   `json:"has_custom_title"`
	Cwd         string `json:"current_directory"`
	Selected    bool   `json:"selected"`
}

// name is what the sidebar shows: the title given at creation, else
// what cmux derived from the directory or the command.
func (w cmuxWorkspace) name() string {
	if w.HasCustom {
		return w.CustomTitle
	}
	return w.Title
}

func (w cmuxWorkspace) workspace() Workspace {
	return Workspace{ID: w.ID, Name: w.name(), Cwd: w.Cwd, Selected: w.Selected}
}

type cmuxList struct {
	Window     string          `json:"window_ref"`
	Workspaces []cmuxWorkspace `json:"workspaces"`
}

func (c Cmux) list() (cmuxList, error) {
	var r cmuxList
	err := c.runJSON(&r, "workspace", "list")
	return r, err
}

func (c Cmux) Workspaces() ([]Workspace, error) {
	r, err := c.list()
	if err != nil {
		return nil, err
	}
	ws := make([]Workspace, 0, len(r.Workspaces))
	for _, w := range r.Workspaces {
		ws = append(ws, w.workspace())
	}
	return ws, nil
}

var cmuxRef = regexp.MustCompile(`\b(workspace|surface|pane):\d+\b`)

// Create makes the workspace without taking the focus. cmux answers
// with a ref; the UUID comes from the list.
func (c Cmux) Create(name, cwd string) (Workspace, error) {
	out, err := c.run("workspace", "create", "--name", name, "--cwd", cwd, "--focus", "false")
	if err != nil {
		return Workspace{}, err
	}
	ref := cmuxRef.FindString(out)
	if ref == "" {
		return Workspace{}, fmt.Errorf("cmux workspace create: no workspace in the answer %q", out)
	}
	r, err := c.list()
	if err != nil {
		return Workspace{}, err
	}
	for _, w := range r.Workspaces {
		if w.Ref == ref {
			return w.workspace(), nil
		}
	}
	return Workspace{}, fmt.Errorf("cmux: created %s, but the list has no such workspace", ref)
}

type cmuxPane struct {
	Ref         string   `json:"ref"`
	SurfaceIDs  []string `json:"surface_ids"`
	SurfaceRefs []string `json:"surface_refs"`
}

func (c Cmux) panes(ws Workspace) ([]cmuxPane, error) {
	var r struct {
		Panes []cmuxPane `json:"panes"`
	}
	if err := c.runJSON(&r, "list-panes", "--workspace", ws.ID); err != nil {
		return nil, err
	}
	if len(r.Panes) == 0 {
		return nil, fmt.Errorf("cmux: workspace %q has no pane", ws.Name)
	}
	return r.Panes, nil
}

// Panes are the workspace's terminal surfaces, pane by pane, each
// pane's tabs in order.
func (c Cmux) Panes(ws Workspace) ([]Pane, error) {
	list, err := c.panes(ws)
	if err != nil {
		return nil, err
	}
	var panes []Pane
	for _, p := range list {
		for _, id := range p.SurfaceIDs {
			panes = append(panes, Pane{ID: id})
		}
	}
	if len(panes) == 0 {
		return nil, fmt.Errorf("cmux: workspace %q has no surface", ws.Name)
	}
	return panes, nil
}

// Layout: the first pane's surfaces are the tabs; the other panes,
// cmux's workspace-wide splits, are listed as the first tab's panes.
func (c Cmux) Layout(ws Workspace) ([]Tab, error) {
	list, err := c.panes(ws)
	if err != nil {
		return nil, err
	}
	var titles struct {
		Surfaces []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"surfaces"`
	}
	if err := c.runJSON(&titles, "list-pane-surfaces", "--workspace", ws.ID, "--pane", list[0].Ref); err != nil {
		return nil, err
	}
	var tabs []Tab
	for i, id := range list[0].SurfaceIDs {
		tab := Tab{Panes: []Pane{{ID: id}}}
		for _, s := range titles.Surfaces {
			if s.ID == id {
				tab.Name = s.Title
			}
		}
		if i == 0 {
			for _, p := range list[1:] {
				if len(p.SurfaceIDs) > 0 {
					tab.Panes = append(tab.Panes, Pane{ID: p.SurfaceIDs[0]})
				}
			}
		}
		tabs = append(tabs, tab)
	}
	return tabs, nil
}

// AgentPane is the first surface, the one the workspace was created
// with: cmux reports agents per workspace, not per surface.
func (c Cmux) AgentPane(ws Workspace) (Pane, error) {
	panes, err := c.Panes(ws)
	if err != nil {
		return Pane{}, err
	}
	return panes[0], nil
}

// surfaceByRef finds the UUID of a surface cmux just named by ref.
func (c Cmux) surfaceByRef(ws Workspace, ref string) (Pane, error) {
	list, err := c.panes(ws)
	if err != nil {
		return Pane{}, err
	}
	for _, p := range list {
		for i, r := range p.SurfaceRefs {
			if r == ref && i < len(p.SurfaceIDs) {
				return Pane{ID: p.SurfaceIDs[i]}, nil
			}
		}
	}
	return Pane{}, fmt.Errorf("cmux: no surface %s in %q", ref, ws.Name)
}

// AddTab adds a surface to the first pane; cmux has no directory
// option for it, so the shell is told to change directory, and the
// tab is titled when a name is given.
func (c Cmux) AddTab(ws Workspace, name, cwd string) (Pane, error) {
	list, err := c.panes(ws)
	if err != nil {
		return Pane{}, err
	}
	out, err := c.run("new-surface", "--type", "terminal", "--workspace", ws.ID, "--pane", list[0].Ref, "--focus", "false")
	if err != nil {
		return Pane{}, err
	}
	pane, err := c.surfaceByRef(ws, cmuxRef.FindString(out))
	if err != nil {
		return Pane{}, err
	}
	if name != "" {
		if _, err := c.run("rename-tab", "--workspace", ws.ID, "--surface", pane.ID, name); err != nil {
			return Pane{}, err
		}
	}
	return pane, c.enter(ws, pane, cwd)
}

// Split splits the surface's pane; the new pane's surface is returned.
func (c Cmux) Split(ws Workspace, pane Pane, dir Direction, cwd string) (Pane, error) {
	out, err := c.run("new-split", string(dir), "--workspace", ws.ID, "--surface", pane.ID, "--focus", "false")
	if err != nil {
		return Pane{}, err
	}
	p, err := c.surfaceByRef(ws, cmuxRef.FindString(out))
	if err != nil {
		return Pane{}, err
	}
	return p, c.enter(ws, p, cwd)
}

// enter moves a fresh shell to cwd, the one way cmux offers.
func (c Cmux) enter(ws Workspace, pane Pane, cwd string) error {
	if cwd == "" {
		return nil
	}
	return c.typeLine(pane, "cd '"+strings.ReplaceAll(cwd, "'", `'\''`)+"'")
}

// Processes are the names cmux's top attributes to the surface's pane.
// cmux files every process of a pane under the pane's first surface,
// whichever tab runs it, so this is as fine as it gets; an idle shell
// may not be listed at all.
func (c Cmux) Processes(ws Workspace, pane Pane) ([]string, error) {
	list, err := c.panes(ws)
	if err != nil {
		return nil, err
	}
	first := ""
	for _, p := range list {
		for _, id := range p.SurfaceIDs {
			if id == pane.ID && len(p.SurfaceRefs) > 0 {
				first = p.SurfaceRefs[0]
			}
		}
	}
	if first == "" {
		return nil, fmt.Errorf("cmux: no surface %s in %q", pane.ID, ws.Name)
	}
	// The rows, tab-separated: cpu, memory, count, kind, ref, parent, name.
	out, err := c.run("top", "--workspace", ws.ID, "--processes", "--format", "tsv")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) >= 7 && f[3] == "process" && f[5] == first {
			names = append(names, f[6])
		}
	}
	return names, nil
}

// AtShell asks cmux what runs in the workspace: a coding agent it has
// detected by process, or the processes of the surface's pane. The
// shell alone, or nothing, means the agent has exited.
func (c Cmux) AtShell(ws Workspace, pane Pane) bool {
	var top struct {
		Agents []struct {
			ID string `json:"id"`
		} `json:"coding_agents"`
	}
	if err := c.runJSON(&top, "top", "--workspace", ws.ID, "--processes"); err != nil || len(top.Agents) > 0 {
		return false
	}
	names, err := c.Processes(ws, pane)
	if err != nil {
		return false
	}
	for _, n := range names {
		if !IsShell(n) {
			return false
		}
	}
	return true
}

// Run types a command line into the surface's shell, with what cmux's
// Claude Code wrapper needs in front. cmux 0.64.22 puts
// CMUX_WORKSPACE_ID and CMUX_TAB_ID in a terminal's environment but
// not CMUX_SURFACE_ID, the one variable the wrapper checks before
// injecting its hooks; without them the state stays unknown. A cmux
// that gives this process the variable gives every terminal the
// variable, and the line is typed as it is.
func (c Cmux) Run(_ Workspace, pane Pane, line string) error {
	if os.Getenv("CMUX_SURFACE_ID") == "" {
		line = "CMUX_SURFACE_ID=" + pane.ID + " " + line
	}
	return c.typeLine(pane, line)
}

// Prompt types the text to the agent, as tmux would: cmux has no
// prompt call of its own for a terminal agent.
func (c Cmux) Prompt(_ Workspace, pane Pane, text string) error { return c.typeLine(pane, text) }

// typeLine types a line and Enter as one submission.
func (c Cmux) typeLine(pane Pane, line string) error {
	if _, err := c.run("send", "--surface", pane.ID, line); err != nil {
		return err
	}
	_, err := c.run("send-key", "--surface", pane.ID, "enter")
	return err
}

// cmuxSession is a record of cmux's Claude Code hook store: one per
// `claude` its wrapper started, kept after the agent exits.
type cmuxSession struct {
	Workspace string `json:"workspace_id"`
	Lifecycle string `json:"agent_lifecycle"` // running, idle, needsInput, unknown
	PIDExists bool   `json:"stored_pid_exists"`
	UpdatedAt string `json:"updated_at"`
}

// unread lists the workspaces with a notification not yet read: cmux
// posts one when Claude finishes or needs permission.
func (c Cmux) unread() map[string]bool {
	var notes []struct {
		Workspace string `json:"workspace_id"`
		Read      bool   `json:"is_read"`
	}
	if err := c.runJSON(&notes, "list-notifications"); err != nil {
		return nil
	}
	m := map[string]bool{}
	for _, n := range notes {
		if !n.Read {
			m[n.Workspace] = true
		}
	}
	return m
}

// States maps cmux's words onto this package's, from the hook store's
// live records: running is working, needsInput is blocked, idle is
// done while cmux's notification about the finished turn is unread
// and idle once it has been read. A workspace without a live record —
// no agent, or one the wrapper never saw — is "".
func (c Cmux) States() (map[string]string, error) {
	r, err := c.list()
	if err != nil {
		return nil, err
	}
	var store struct {
		Sessions []cmuxSession `json:"sessions"`
	}
	live := map[string]cmuxSession{}
	if err := c.runJSON(&store, "sessions", "--agent", "claude"); err == nil {
		for _, s := range store.Sessions {
			if s.PIDExists && s.UpdatedAt >= live[s.Workspace].UpdatedAt {
				live[s.Workspace] = s
			}
		}
	}
	var unread map[string]bool
	states := map[string]string{}
	for _, w := range r.Workspaces {
		states[w.name()] = ""
		s, ok := live[w.ID]
		if !ok {
			continue
		}
		switch s.Lifecycle {
		case "running":
			states[w.name()] = Working
		case "needsInput":
			states[w.name()] = Blocked
		case "idle":
			if unread == nil {
				unread = c.unread()
			}
			if unread[w.ID] {
				states[w.name()] = Done
			} else {
				states[w.name()] = Idle
			}
		}
	}
	return states, nil
}

// Select shows the workspace in its window and, the user arriving,
// marks its notifications read: done becomes idle.
func (c Cmux) Select(ws Workspace) error {
	if _, err := c.run("workspace", "select", "--workspace", ws.ID); err != nil {
		return err
	}
	_, _ = c.run("mark-notification-read", "--workspace", ws.ID)
	return nil
}

// Focus brings cmux's window to the front, for a process running
// outside it; inside, the window is already there.
func (c Cmux) Focus() error {
	if c.Inside() {
		return nil
	}
	r, err := c.list()
	if err != nil {
		return err
	}
	_, err = c.run("focus-window", "--window", r.Window)
	return err
}

func (c Cmux) Close(ws Workspace) error {
	_, err := c.run("workspace", "close", "--workspace", ws.ID)
	return err
}

// Current is the workspace of the terminal this process runs in.
func (c Cmux) Current() (Workspace, bool) {
	id := os.Getenv("CMUX_WORKSPACE_ID")
	if id == "" {
		return Workspace{}, false
	}
	r, err := c.list()
	if err != nil {
		return Workspace{}, false
	}
	for _, w := range r.Workspaces {
		if w.ID == id {
			return w.workspace(), true
		}
	}
	return Workspace{}, false
}

func (Cmux) Describe(ws Workspace) string { return ws.Name }

func (c Cmux) AttachHint() string {
	if c.Inside() {
		return ""
	}
	return "cmux"
}

func (c Cmux) Notify(title, text string) {
	args := []string{"notify", "--title", title, "--body", text}
	if id := os.Getenv("CMUX_WORKSPACE_ID"); id != "" {
		args = append(args, "--workspace", id)
	}
	_, _ = c.run(args...)
}

// Session: cmux has no container above its workspaces.
func (Cmux) Session() string { return "" }

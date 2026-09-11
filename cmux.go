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
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Cmux drives the cmux application through its CLI. Handles are
// cmux's UUIDs, which survive reordering; refs like workspace:2 do not.
//
// Every read comes from a snapshot — the workspace list, the whole
// tree of panes and surfaces, the whole process table — taken with
// three cmux commands and kept until the driver changes something.
// NewCmux keeps the snapshot across calls; the zero value takes a
// fresh one for every call, right but slow.
type Cmux struct{ cache *cmuxSnapshot }

// NewCmux returns a driver that reads cmux once per run.
func NewCmux() Cmux { return Cmux{cache: &cmuxSnapshot{}} }

// cmuxSnapshot is what the driver has read of cmux so far.
type cmuxSnapshot struct {
	list   *cmuxList
	tree   *cmuxTree
	ps     *psTable
	pills  map[string]string // workspace id → claude-status's pill; nil until read
	groups *[]cmuxGroup      // the sidebar's groups; nil until read
	watch  *cmuxWatch        // the live event subscription, when Watch started one
}

// snapshot is where reads go: the kept one, or a throwaway.
func (c Cmux) snapshot() *cmuxSnapshot {
	if c.cache != nil {
		return c.cache
	}
	return &cmuxSnapshot{}
}

// changed forgets the snapshot: the next read sees what cmux does. A
// live watch survives it — the subscription is not a cached read — but
// is told, because our own write changes what States() should answer
// and cmux's event for it may arrive a moment later.
func (c Cmux) changed() {
	if c.cache == nil {
		return
	}
	w := c.cache.watch
	*c.cache = cmuxSnapshot{watch: w}
	if w != nil {
		w.stale(cmuxStaleAll)
	}
}

// watcher is the live subscription, or nil when there is none.
func (c Cmux) watcher() *cmuxWatch {
	if c.cache == nil {
		return nil
	}
	return c.cache.watch
}

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
// cmux unless cmux was launched with CMUX_SOCKET_MODE=allowAll. It
// then reads the app's environment: TMUX in it makes cmux's shell
// integration hand CMUX_SURFACE_ID to tmux before every command, so
// the Claude Code hooks never engage, and Claude Code's session markers
// make every agent a child session — both come from an app launched
// out of a shell, and both are fixed by relaunching it from a hotkey.
func (c Cmux) Ping() error {
	if _, err := c.run("ping"); err != nil {
		if c.Inside() {
			return err
		}
		return fmt.Errorf("%w (run from a cmux terminal, or start cmux with CMUX_SOCKET_MODE=allowAll)", err)
	}
	var id struct {
		Socket string `json:"socket_path"`
	}
	if err := c.runJSON(&id, "identify"); err != nil || id.Socket == "" {
		return nil // the socket answered; no path to find its owner by
	}
	return auditCmuxApp(id.Socket)
}

// auditCmuxApp reads the environment of the app holding the socket.
func auditCmuxApp(socket string) error {
	env, ok := ownerEnv(socket)
	if !ok {
		return nil
	}
	if len(carries(env, "TMUX")) > 0 {
		return taintError{"cmux: the app was launched with TMUX in its environment (from a shell inside tmux): its shell integration hands CMUX_SURFACE_ID to tmux before every command and the Claude Code hooks never engage; relaunch cmux from a hotkey or Spotlight"}
	}
	return claudeTaint("cmux: the app", env)
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
	snap := c.snapshot()
	if snap.list == nil {
		var r cmuxList
		if err := c.runJSON(&r, "workspace", "list"); err != nil {
			return cmuxList{}, err
		}
		snap.list = &r
	}
	return *snap.list, nil
}

// cmuxTree is `tree --all`: every window's workspaces, their panes,
// their surfaces — the layout of everything in one answer.
type cmuxTree struct {
	Windows []struct {
		Workspaces []struct {
			ID    string     `json:"id"`
			Ref   string     `json:"ref"`
			Panes []cmuxPane `json:"panes"`
		} `json:"workspaces"`
	} `json:"windows"`
}

func (c Cmux) tree() (*cmuxTree, error) {
	snap := c.snapshot()
	if snap.tree == nil {
		var t cmuxTree
		if err := c.runJSON(&t, "tree", "--all"); err != nil {
			return nil, err
		}
		snap.tree = &t
	}
	return snap.tree, nil
}

// psTable is `ps -axo tty=,stat=,comm=`: every process with its tty,
// state and command, read once. A `+` in the state marks a foreground
// process — what a keystroke would reach.
type psTable struct {
	rows [][3]string
}

func (c Cmux) ps() (*psTable, error) {
	snap := c.snapshot()
	if snap.ps == nil {
		out, err := runOut(exec.Command("ps", "-axo", "tty=,stat=,comm="))
		if err != nil {
			return nil, err
		}
		t := &psTable{}
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) >= 3 {
				t.rows = append(t.rows, [3]string{f[0], f[1], strings.Join(f[2:], " ")})
			}
		}
		snap.ps = t
	}
	return snap.ps, nil
}

// foreground names the processes in the foreground on a tty, by
// their command's base name.
func (t *psTable) foreground(tty string) []string {
	var names []string
	for _, r := range t.rows {
		if r[0] == tty && strings.Contains(r[1], "+") {
			names = append(names, filepath.Base(r[2]))
		}
	}
	return names
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
	c.changed()
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
	Ref         string        `json:"ref"`
	SurfaceIDs  []string      `json:"surface_ids"`
	SurfaceRefs []string      `json:"surface_refs"`
	Surfaces    []cmuxSurface `json:"surfaces"`
}

// cmuxSurface is a terminal surface as the tree reports it. cmux
// knows the tty of the surface a workspace was created with; a tab or
// split made through its API has none, and its title is what cmux's
// shell integration last reported: the running program, or the
// directory at a prompt.
type cmuxSurface struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	TTY   string `json:"tty"`
}

// atPrompt reads a title as a shell at its prompt: a directory, or the
// name a surface has before its first prompt.
func (s cmuxSurface) atPrompt() bool {
	t := s.Title
	return t == "" || t == "Terminal" || t == "~" || strings.HasPrefix(t, "~/") || strings.HasPrefix(t, "/") || strings.HasPrefix(t, "…/")
}

// programs names what the title says runs there: its words, each by
// base name, so `cd /x && nvim` names nvim among them.
func (s cmuxSurface) programs() []string {
	if s.atPrompt() {
		return nil
	}
	var names []string
	for _, w := range strings.Fields(s.Title) {
		names = append(names, filepath.Base(w))
	}
	return names
}

func (c Cmux) panes(ws Workspace) ([]cmuxPane, error) {
	t, err := c.tree()
	if err != nil {
		return nil, err
	}
	for _, w := range t.Windows {
		for _, x := range w.Workspaces {
			if x.ID == ws.ID || x.Ref == ws.ID {
				if len(x.Panes) == 0 {
					return nil, fmt.Errorf("cmux: workspace %q has no pane", ws.Name)
				}
				return x.Panes, nil
			}
		}
	}
	return nil, fmt.Errorf("cmux: no workspace %q", ws.Name)
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
	var tabs []Tab
	for i, id := range list[0].SurfaceIDs {
		tab := Tab{Panes: []Pane{{ID: id}}}
		for _, s := range list[0].Surfaces {
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
	c.changed()
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
	c.changed()
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

// surface finds a surface of the workspace in the tree.
func (c Cmux) surface(ws Workspace, pane Pane) (cmuxSurface, error) {
	list, err := c.panes(ws)
	if err != nil {
		return cmuxSurface{}, err
	}
	for _, p := range list {
		for _, s := range p.Surfaces {
			if s.ID == pane.ID {
				return s, nil
			}
		}
	}
	return cmuxSurface{}, fmt.Errorf("cmux: no surface %s in %q", pane.ID, ws.Name)
}

// Processes names what runs in the surface: the foreground processes
// on its tty when cmux knows the tty, else what its title says.
func (c Cmux) Processes(ws Workspace, pane Pane) ([]string, error) {
	s, err := c.surface(ws, pane)
	if err != nil {
		return nil, err
	}
	if s.TTY != "" {
		t, err := c.ps()
		if err != nil {
			return nil, err
		}
		return t.foreground(strings.TrimPrefix(s.TTY, "/dev/")), nil
	}
	return s.programs(), nil
}

// AtShell: nothing, or nothing but shells, runs in the surface.
func (c Cmux) AtShell(ws Workspace, pane Pane) bool {
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

// Run types a command line into the surface's shell. cmux gives its
// terminals the CMUX_SURFACE_ID its Claude Code wrapper needs; a cmux
// where it is missing is broken, and Ping says so.
func (c Cmux) Run(_ Workspace, pane Pane, line string) error { return c.typeLine(pane, line) }

// Prompt types the text to the agent, as tmux would: cmux has no
// prompt call of its own for a terminal agent.
func (c Cmux) Prompt(_ Workspace, pane Pane, text string) error { return c.typeLine(pane, text) }

// typeLine types a line and Enter as one submission.
func (c Cmux) typeLine(pane Pane, line string) error {
	if _, err := c.run("send", "--surface", pane.ID, line); err != nil {
		return err
	}
	_, err := c.run("send-key", "--surface", pane.ID, "enter")
	c.changed() // what runs there is about to change
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

// claudePill is the sidebar status pill claude-status's hooks keep per
// workspace: its key, and the four words it holds.
const claudePill = "claude"

// pills reads claude-status's pill of every workspace, one list-status
// per workspace, run side by side: a call costs ~0.15 s of cmux CLI
// start-up, the same for one as for fifteen in parallel. A workspace
// without the pill — no Claude there, or the plugin not installed — is
// absent from the map; so is one whose read failed, closed meanwhile.
func (c Cmux) pills() (map[string]string, error) {
	snap := c.snapshot()
	if snap.pills != nil {
		return snap.pills, nil
	}
	r, err := c.list()
	if err != nil {
		return nil, err
	}
	pills := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, w := range r.Workspaces {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			out, err := c.run("list-status", "--workspace", id)
			if err != nil {
				return
			}
			if v, ok := parsePill(out, claudePill); ok {
				mu.Lock()
				pills[id] = v
				mu.Unlock()
			}
		}(w.ID)
	}
	wg.Wait()
	snap.pills = pills
	return pills, nil
}

// parsePill finds key's value in list-status output: one pill per
// line as `key=value icon=NAME color=#HEX`, plus `priority=N` for one
// set with a priority; the value is unquoted (cmux's own
// `claude_code=Needs input …` has a space in it), so it runs up to
// ` icon=`. No pills is the single line `No status entries`.
func parsePill(out, key string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, key+"=") {
			continue
		}
		v := strings.TrimPrefix(line, key+"=")
		if i := strings.Index(v, " icon="); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v), true
	}
	return "", false
}

// cmuxGroup is one `workspace-group list` record: the collapsible
// sidebar container, owned by the anchor workspace whose row is its
// header. Closing the anchor promotes the next member, and the last
// member leaving takes the group with it, so nothing has to be
// cleaned up on the close path.
type cmuxGroup struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	AnchorID  string   `json:"anchor_workspace_id"`
	MemberIDs []string `json:"member_workspace_ids"`
}

// groups reads the sidebar's groups, once per run like the other
// reads.
func (c Cmux) groups() ([]cmuxGroup, error) {
	snap := c.snapshot()
	if snap.groups != nil {
		return *snap.groups, nil
	}
	var r struct {
		Groups []cmuxGroup `json:"groups"`
	}
	if err := c.runJSON(&r, "workspace-group", "list"); err != nil {
		return nil, err
	}
	snap.groups = &r.Groups
	return r.Groups, nil
}

// Group puts the workspace in the named group, creating that group
// around it the first time.
//
// cmux 0.64.22 makes an anchor of its own whatever `create --from`
// says: a workspace titled after the group, in the home directory,
// listed first among the members. Left alone it would put a phantom
// row in the sidebar for every group, and hold the group open after
// the last real member closed. So a create is followed by moving the
// anchor onto the caller's workspace and closing the one cmux made
// (see adoptAnchor).
//
// The group is looked up by name, which is what the CLI offers:
// `workspace-group list` prints no stable key of our own (the record
// carries an external id, but only over raw rpc, and rpc's create is
// the call that makes the phantom). So renaming a group in the sidebar
// hides it from us, and the next call makes a second group under the
// old name. That is the cost of the documented path, and a rename is
// the user saying they want their own arrangement.
//
// Style is best effort. An unknown SF Symbol is stored as given —
// cmux trims the name and keeps whatever it is handed — but a refused
// colour or icon must not fail an open: the workspace is grouped
// either way, and that is what the caller asked for.
func (c Cmux) Group(name string, ws Workspace, style GroupStyle) error {
	gs, err := c.groups()
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.Name != name {
			continue
		}
		for _, id := range g.MemberIDs {
			if id == ws.ID {
				return nil // already there
			}
		}
		if _, err := c.run("workspace-group", "add", "--group", g.ID, "--workspace", ws.ID); err != nil {
			return err
		}
		c.changed()
		return nil
	}
	if _, err := c.run("workspace-group", "create", "--name", name, "--from", ws.ID); err != nil {
		return err
	}
	c.changed()
	c.adoptAnchor(name, ws)
	c.style(name, style)
	return nil
}

// adoptAnchor makes ws the anchor of the group just created for it and
// closes the anchor cmux generated, so the sidebar shows the group on
// the caller's own workspace and loses the group when that workspace
// closes.
//
// Closing a workspace cannot be undone, so this only fires on the one
// shape a fresh create leaves behind: a group of exactly two, the
// caller's workspace and an anchor that is not it. Anything else — a
// third member, an anchor that is already ours, a group that cannot be
// found — is left as it is. An untidy sidebar is a smaller price than
// closing something of the user's.
//
// The generated anchor is inferred from that shape. cmux's rpc records
// carry `anchor_workspace_is_generated`, which would say it outright,
// but 0.64.22's `workspace-group list` does not print it. When a later
// cmux does, read the flag instead of counting members.
func (c Cmux) adoptAnchor(name string, ws Workspace) {
	gs, err := c.groups()
	if err != nil {
		return
	}
	for _, g := range gs {
		if g.Name != name {
			continue
		}
		if len(g.MemberIDs) != 2 || g.AnchorID == ws.ID || g.AnchorID == "" {
			return
		}
		if !slices.Contains(g.MemberIDs, ws.ID) || !slices.Contains(g.MemberIDs, g.AnchorID) {
			return
		}
		generated := g.AnchorID
		if _, err := c.run("workspace-group", "set-anchor", "--group", g.ID, "--workspace", ws.ID); err != nil {
			return // the group stands; its header is just cmux's row
		}
		c.changed()
		_, _ = c.run("workspace", "close", "--workspace", generated)
		c.changed()
		return
	}
}

// style paints a group the caller named, quietly: the answer to
// create is not parsed for a handle, the fresh list is read for one.
func (c Cmux) style(name string, style GroupStyle) {
	if style.Color == "" && style.Icon == "" {
		return
	}
	gs, err := c.groups()
	if err != nil {
		return
	}
	for _, g := range gs {
		if g.Name != name {
			continue
		}
		if style.Color != "" {
			_, _ = c.run("workspace-group", "set-color", g.ID, "--hex", style.Color)
		}
		if style.Icon != "" {
			_, _ = c.run("workspace-group", "set-icon", g.ID, "--symbol", style.Icon)
		}
		return
	}
}

// States speaks this package's words for every workspace. The first
// word is claude-status's: its hooks write a pill (`claude=working`,
// `blocked`, `done`, `idle`) from the events that mean those things,
// and `done` stays done while cmux's notification about the turn is
// unread, idle once it has been read (cmux clears it on a visit; Seen
// marks it). Without the pill the hook store's live record decides:
// running is working, needsInput is blocked, idle is done or idle by
// the same notification. That record is the fallback, not the source,
// because cmux folds Claude Code's idle reminder — the notification
// sent 60 seconds after every finished turn — into needsInput (its
// AgentHookNotificationPolicy: `.needsPermission, .idleReminder →
// .needsInput`), so most idle sessions read as blocked there. A
// workspace with neither — no agent, or one the wrapper never saw —
// is "".
func (c Cmux) States() (map[string]string, error) {
	// A live watch already holds all four, kept current by cmux's
	// events, so the answer costs nothing to give.
	if w := c.watcher(); w != nil {
		v := w.snapshot()
		return statesFrom(v.list, v.pills, v.sessions, func(id string) bool { return !v.unread[id] }), nil
	}
	list, err := c.list()
	if err != nil {
		return nil, err
	}
	pills, err := c.pills()
	if err != nil {
		return nil, err
	}
	// The notifications are read only if a done or an idle asks: they
	// answer nothing else, and a list of plain states should not pay
	// for them.
	var unread map[string]bool
	seen := func(id string) bool {
		if unread == nil {
			unread = c.unread()
		}
		return !unread[id]
	}
	return statesFrom(list, pills, c.sessions(), seen), nil
}

// sessions is cmux's hook store by workspace: the latest record whose
// process still exists, which is the only one that says anything about
// now.
func (c Cmux) sessions() map[string]cmuxSession {
	live := map[string]cmuxSession{}
	var store struct {
		Sessions []cmuxSession `json:"sessions"`
	}
	if err := c.runJSON(&store, "sessions", "--agent", "claude"); err != nil {
		return live
	}
	for _, s := range store.Sessions {
		if s.PIDExists && s.UpdatedAt >= live[s.Workspace].UpdatedAt {
			live[s.Workspace] = s
		}
	}
	return live
}

// statesFrom is the reading itself, over what has already been read:
// the same answer whether a watch keeps those four or a poll just
// fetched them.
func statesFrom(list cmuxList, pills map[string]string, live map[string]cmuxSession, seen func(id string) bool) map[string]string {
	states := map[string]string{}
	for _, w := range list.Workspaces {
		states[w.name()] = ""
		if pill, ok := pills[w.ID]; ok {
			switch pill {
			case Working, Blocked, Idle:
				states[w.name()] = pill
			case Done:
				if seen(w.ID) {
					states[w.name()] = Idle
				} else {
					states[w.name()] = Done
				}
			}
			continue
		}
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
			if seen(w.ID) {
				states[w.name()] = Idle
			} else {
				states[w.name()] = Done
			}
		}
	}
	return states
}

// Select shows the workspace in its window.
func (c Cmux) Select(ws Workspace) error {
	_, err := c.run("workspace", "select", "--workspace", ws.ID)
	c.changed()
	return err
}

// Seen marks the workspace's notifications read: done becomes idle.
func (c Cmux) Seen(ws Workspace) error {
	_, err := c.run("mark-notification-read", "--workspace", ws.ID)
	c.changed()
	return err
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
	c.changed()
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

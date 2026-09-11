// Package mux drives the terminal multiplexers that hold coding
// agents: tmux, herdr and cmux. The model is the one all three share —
// a workspace per task, panes inside it, a shell to type a command
// line into, an agent to hand a prompt to, and what the multiplexer
// knows about that agent. Each driver is one file; what it verified
// on the real thing is in its comments.
package mux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// What the agent in a workspace is doing, as the multiplexer reports
// it. "" is unknown: no agent, or one the multiplexer cannot see.
const (
	Working = "working"
	Blocked = "blocked" // waiting for a person: a permission, a question
	Done    = "done"    // finished, not yet looked at
	Idle    = "idle"
)

// Workspace is one task's place: a tmux window of the driver's
// session, a herdr workspace, a cmux workspace.
type Workspace struct {
	ID       string // the multiplexer's own handle
	Name     string // tmux window name, herdr label, cmux title
	Cwd      string // when the multiplexer reports it
	Selected bool
}

// Pane is where lines are typed: a tmux pane, a herdr pane, a cmux
// terminal surface.
type Pane struct {
	ID string
}

// Tab is a workspace's tab with its panes, the root first: a herdr
// tab, a cmux surface of the first pane, the single tab of a tmux
// window. Under cmux a workspace's splits are workspace-wide, and are
// listed as the first tab's panes.
type Tab struct {
	Name  string
	Panes []Pane
}

// Direction of a split.
type Direction string

const (
	Right Direction = "right"
	Down  Direction = "down"
)

// ErrUnsupported is what a driver answers for an operation its
// multiplexer has no counterpart for.
var ErrUnsupported = errors.New("not supported by this multiplexer")

// GroupStyle is how a group is shown where the multiplexer shows it.
type GroupStyle struct {
	Color string // "#RRGGBB"; "" leaves the multiplexer's default
	Icon  string // an SF Symbol name under cmux; "" for none
}

// Grouper is implemented by drivers that can hold workspaces in a
// named, visible container. Only cmux has one: tmux's container is
// the session a driver already holds its windows in, and herdr's API
// has no grouping at all. It is a capability beside Driver rather
// than a verb on it, so the two without it need no stub that lies.
type Grouper interface {
	// Group puts ws in the group called name, creating that group
	// anchored on ws when it does not exist yet, and applying style on
	// creation. An error means the grouping failed; a style the
	// multiplexer refused does not, since a workspace in a plain group
	// is the point and its colour is not.
	Group(name string, ws Workspace, style GroupStyle) error
}

// Group puts ws in a named group when the driver has them, and does
// nothing when it does not — the caller does not branch on Kind.
func Group(d Driver, name string, ws Workspace, style GroupStyle) error {
	g, ok := d.(Grouper)
	if !ok {
		return nil
	}
	return g.Group(name, ws, style)
}

// Watcher is implemented by drivers whose multiplexer can say when
// something changed, instead of being asked again. Only cmux has a
// stream to listen to; tmux has none, and herdr's is not read yet.
type Watcher interface {
	// Watch signals on the returned channel whenever a later States()
	// would answer differently, and keeps the driver's own view current
	// so that States() runs nothing while the watch is live. Signals
	// coalesce: a caller that reads slowly loses the count, never the
	// change. The channel closes when ctx is done.
	Watch(ctx context.Context) (<-chan struct{}, error)
}

// Watch starts a driver's watch when it has one, and answers nil when
// it does not — the caller keeps its timer for those and does not
// branch on Kind. A nil channel blocks forever in a select, which is
// what a caller polling beside it wants.
func Watch(d Driver, ctx context.Context) (<-chan struct{}, error) {
	w, ok := d.(Watcher)
	if !ok {
		return nil, nil
	}
	return w.Watch(ctx)
}

// Driver is one multiplexer.
type Driver interface {
	// Kind names the multiplexer: tmux, herdr, cmux.
	Kind() string
	// Inside reports whether this process runs in the multiplexer.
	Inside() bool
	// ChildEnv is what a child process needs to reach the same
	// multiplexer instance.
	ChildEnv() []string
	// Ping reports whether the multiplexer answers; the error says how
	// to reach it.
	Ping() error
	// Prepare makes the container of workspaces exist — tmux's
	// session — with cwd as its directory; a no-op elsewhere.
	Prepare(cwd string) error
	// Workspaces lists the workspaces, in the multiplexer's order.
	Workspaces() ([]Workspace, error)
	// Create makes a workspace in cwd, without taking the focus.
	Create(name, cwd string) (Workspace, error)
	// Panes lists a workspace's panes in creation order, the root first.
	Panes(ws Workspace) ([]Pane, error)
	// Layout lists the workspace's tabs with their panes, in order, so
	// that a declared layout can be completed rather than repeated.
	Layout(ws Workspace) ([]Tab, error)
	// AgentPane is the pane an agent runs in: the one the multiplexer
	// detects an agent in, else the root.
	AgentPane(ws Workspace) (Pane, error)
	// AddTab adds a tab to the workspace, in cwd: a herdr tab, a cmux
	// surface. tmux has no tabs inside a window: ErrUnsupported.
	AddTab(ws Workspace, name, cwd string) (Pane, error)
	// Split splits a pane, the new one in cwd.
	Split(ws Workspace, pane Pane, dir Direction, cwd string) (Pane, error)
	// Processes names the processes the multiplexer attributes to the
	// pane; nothing for an idle shell where it lists only children.
	Processes(ws Workspace, pane Pane) ([]string, error)
	// AtShell reports whether the pane shows a shell at its prompt, so
	// a typed line runs as a command.
	AtShell(ws Workspace, pane Pane) bool
	// Run types a command line and Enter into the pane's shell.
	Run(ws Workspace, pane Pane, line string) error
	// Prompt hands text to the agent in the pane: the multiplexer's
	// own way when it has one, keystrokes otherwise.
	Prompt(ws Workspace, pane Pane, text string) error
	// States reports each workspace's agent state by workspace name.
	States() (map[string]string, error)
	// Select shows the workspace.
	Select(ws Workspace) error
	// Seen tells the multiplexer the user has looked at the workspace:
	// cmux marks its notifications read, so a done agent reads idle.
	// A no-op elsewhere.
	Seen(ws Workspace) error
	// Focus brings the user's client, or the application, to the front.
	Focus() error
	// Close removes the workspace.
	Close(ws Workspace) error
	// Current is the workspace this process runs in, if any.
	Current() (Workspace, bool)
	// Describe names a workspace the way the user sees it.
	Describe(ws Workspace) string
	// AttachHint tells a user outside the multiplexer how to reach it;
	// "" when they are inside.
	AttachHint() string
	// Notify shows the user a transient message, the multiplexer's way.
	Notify(title, text string)
	// Session names the container the workspaces live in: the tmux
	// session, the herdr session, "" for cmux.
	Session() string
}

// Detect picks the driver for the multiplexer this process runs in,
// herdr or cmux, and tmux otherwise — tmux's own variable says nothing
// about which session the reviews should join, so tmux is the default,
// not a detection.
func Detect(tmux Tmux, herdr Herdr, cmux Cmux) Driver {
	switch {
	case herdr.Inside():
		return herdr
	case cmux.Inside():
		return cmux
	}
	return tmux
}

// ByKind returns the driver named, with its default configuration;
// nil for an unknown kind.
func ByKind(kind string) Driver {
	switch kind {
	case "tmux":
		return Tmux{}
	case "herdr":
		return NewHerdr("")
	case "cmux":
		return NewCmux()
	}
	return nil
}

// IsShell reports whether a process name is a shell's — the agent has
// exited, and a typed prompt would run as a command.
func IsShell(command string) bool {
	switch strings.TrimPrefix(filepath.Base(command), "-") {
	case "sh", "bash", "zsh", "fish", "dash", "ksh", "nu":
		return true
	}
	return false
}

// expandHome replaces a leading ~ with the home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

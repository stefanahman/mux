// herdr as the multiplexer: one herdr workspace per task, its root
// pane running the agent. herdr detects the agent itself and reports
// what it is doing, so no companion is needed for the state.
// Everything goes over herdr's socket: one newline-delimited JSON
// request per connection, its response read back. Shapes are herdr
// 0.9's, seen on the wire.
package mux

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Herdr drives a herdr session through its socket.
type Herdr struct {
	// Socket is the session's socket. NewHerdr resolves it.
	Socket string
}

// NewHerdr finds the socket: the one given, else the one herdr gives
// its panes (HERDR_SOCKET_PATH), else the default session's.
func NewHerdr(socket string) Herdr {
	if socket == "" {
		socket = os.Getenv("HERDR_SOCKET_PATH")
	}
	if socket == "" {
		home, _ := os.UserHomeDir()
		socket = filepath.Join(home, ".config", "herdr", "herdr.sock")
	}
	return Herdr{Socket: expandHome(socket)}
}

// HerdrError is an error herdr answered with; Code is what a caller
// can act on (agent_blocked, not_found, …).
type HerdrError struct{ Code, Message string }

func (e *HerdrError) Error() string { return "herdr: " + e.Message + " (" + e.Code + ")" }

// call sends one request and returns its result.
func (h Herdr) call(method string, params any) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", h.Socket, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("herdr: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	id := "mux/" + method
	req, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("herdr %s: %w", method, err)
	}
	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadBytes('\n')
		var resp struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &resp) == nil && resp.ID == id {
			if resp.Error != nil {
				return nil, &HerdrError{resp.Error.Code, resp.Error.Message}
			}
			return resp.Result, nil
		}
		if err != nil {
			return nil, fmt.Errorf("herdr %s: no answer: %w", method, err)
		}
	}
}

func (Herdr) Kind() string { return "herdr" }

// Inside: herdr sets HERDR_ENV=1 in every pane.
func (Herdr) Inside() bool { return os.Getenv("HERDR_ENV") == "1" }

func (h Herdr) ChildEnv() []string { return []string{"HERDR_SOCKET_PATH=" + h.Socket} }

func (h Herdr) Ping() error {
	_, err := h.call("ping", struct{}{})
	return err
}

// Prepare has nothing to create — herdr has no container above its
// workspaces — but makes sure herdr is there.
func (h Herdr) Prepare(string) error { return h.Ping() }

var herdrSessionSocket = regexp.MustCompile(`/sessions/([^/]+)/herdr\.sock$`)

// Session is the herdr session's name: from the environment herdr
// gives its panes (HERDR_SESSION, seen but not documented), else from
// the socket's path, else the default session.
func (h Herdr) Session() string {
	if s := os.Getenv("HERDR_SESSION"); s != "" {
		return s
	}
	if m := herdrSessionSocket.FindStringSubmatch(h.Socket); m != nil {
		return m[1]
	}
	return "default"
}

type herdrWorkspace struct {
	ID      string `json:"workspace_id"`
	Label   string `json:"label"`
	Status  string `json:"agent_status"`
	Focused bool   `json:"focused"`
}

func (h Herdr) list() ([]herdrWorkspace, error) {
	raw, err := h.call("workspace.list", struct{}{})
	if err != nil {
		return nil, err
	}
	var r struct {
		Workspaces []herdrWorkspace `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("herdr workspace.list: %w", err)
	}
	return r.Workspaces, nil
}

func (h Herdr) Workspaces() ([]Workspace, error) {
	list, err := h.list()
	if err != nil {
		return nil, err
	}
	ws := make([]Workspace, 0, len(list))
	for _, w := range list {
		ws = append(ws, Workspace{ID: w.ID, Name: w.Label, Selected: w.Focused})
	}
	return ws, nil
}

// Create makes the workspace without taking the focus.
func (h Herdr) Create(name, cwd string) (Workspace, error) {
	raw, err := h.call("workspace.create", map[string]any{"cwd": cwd, "label": name, "focus": false})
	if err != nil {
		return Workspace{}, err
	}
	var r struct {
		Workspace struct {
			ID string `json:"workspace_id"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Workspace.ID == "" {
		return Workspace{}, fmt.Errorf("herdr workspace.create: no workspace in the answer")
	}
	return Workspace{ID: r.Workspace.ID, Name: name, Cwd: cwd}, nil
}

type herdrPane struct {
	ID    string `json:"pane_id"`
	Tab   string `json:"tab_id"`
	Agent string `json:"agent"`
}

// panes lists the workspace's panes in creation order, the root first
// — herdr lists them so without promising to.
func (h Herdr) panes(ws Workspace) ([]herdrPane, error) {
	raw, err := h.call("pane.list", map[string]any{"workspace_id": ws.ID})
	if err != nil {
		return nil, err
	}
	var r struct {
		Panes []herdrPane `json:"panes"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Panes) == 0 {
		return nil, fmt.Errorf("herdr: workspace %q has no pane", ws.Name)
	}
	return r.Panes, nil
}

func (h Herdr) Panes(ws Workspace) ([]Pane, error) {
	list, err := h.panes(ws)
	if err != nil {
		return nil, err
	}
	panes := make([]Pane, 0, len(list))
	for _, p := range list {
		panes = append(panes, Pane{ID: p.ID})
	}
	return panes, nil
}

// Layout groups the panes by tab, tabs in creation order.
func (h Herdr) Layout(ws Workspace) ([]Tab, error) {
	raw, err := h.call("tab.list", map[string]any{"workspace_id": ws.ID})
	if err != nil {
		return nil, err
	}
	var r struct {
		Tabs []struct {
			ID    string `json:"tab_id"`
			Label string `json:"label"`
		} `json:"tabs"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("herdr tab.list: %w", err)
	}
	panes, err := h.panes(ws)
	if err != nil {
		return nil, err
	}
	tabs := make([]Tab, 0, len(r.Tabs))
	for _, t := range r.Tabs {
		tab := Tab{Name: t.Label}
		for _, p := range panes {
			if p.Tab == t.ID {
				tab.Panes = append(tab.Panes, Pane{ID: p.ID})
			}
		}
		tabs = append(tabs, tab)
	}
	return tabs, nil
}

// AgentPane is the pane herdr detects an agent in, else the root.
func (h Herdr) AgentPane(ws Workspace) (Pane, error) {
	list, err := h.panes(ws)
	if err != nil {
		return Pane{}, err
	}
	for _, p := range list {
		if p.Agent != "" {
			return Pane{ID: p.ID}, nil
		}
	}
	return Pane{ID: list[0].ID}, nil
}

// AddTab creates a tab labelled name; its root pane is returned.
func (h Herdr) AddTab(ws Workspace, name, cwd string) (Pane, error) {
	params := map[string]any{"workspace_id": ws.ID, "label": name}
	if cwd != "" {
		params["cwd"] = cwd
	}
	raw, err := h.call("tab.create", params)
	if err != nil {
		return Pane{}, err
	}
	var r struct {
		Tab struct {
			ID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			ID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.RootPane.ID == "" {
		return Pane{}, fmt.Errorf("herdr tab.create: no root pane in the answer")
	}
	return Pane{ID: r.RootPane.ID}, nil
}

// Split splits the pane; the new one does not take the focus. The
// pane is named as target_pane_id (the schema's word; pane_id is
// silently ignored and the focused pane split instead).
func (h Herdr) Split(ws Workspace, pane Pane, dir Direction, cwd string) (Pane, error) {
	params := map[string]any{"workspace_id": ws.ID, "target_pane_id": pane.ID, "direction": string(dir), "focus": false}
	if cwd != "" {
		params["cwd"] = cwd
	}
	raw, err := h.call("pane.split", params)
	if err != nil {
		return Pane{}, err
	}
	var r struct {
		Pane struct {
			ID string `json:"pane_id"`
		} `json:"pane"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Pane.ID == "" {
		return Pane{}, fmt.Errorf("herdr pane.split: no pane in the answer")
	}
	return Pane{ID: r.Pane.ID}, nil
}

// Processes are the pane's foreground processes, the front one first.
func (h Herdr) Processes(_ Workspace, pane Pane) ([]string, error) {
	raw, err := h.call("pane.process_info", map[string]any{"pane_id": pane.ID})
	if err != nil {
		return nil, err
	}
	var r struct {
		Info struct {
			Foreground []struct {
				Name string `json:"name"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("herdr pane.process_info: %w", err)
	}
	names := make([]string, 0, len(r.Info.Foreground))
	for _, p := range r.Info.Foreground {
		names = append(names, p.Name)
	}
	return names, nil
}

// AtShell reads the foreground process, never the detected state:
// herdr's state flickers through idle mid-turn.
func (h Herdr) AtShell(ws Workspace, pane Pane) bool {
	ps, err := h.Processes(ws, pane)
	return err == nil && len(ps) > 0 && IsShell(ps[0])
}

// Run types a line and Enter as one submission.
func (h Herdr) Run(_ Workspace, pane Pane, line string) error {
	_, err := h.call("pane.send_input", map[string]any{"pane_id": pane.ID, "text": line, "keys": []string{"enter"}})
	return err
}

// Prompt hands the text to herdr's agent.prompt, which refuses on its
// own while the agent is blocked, so keystrokes never answer a dialog.
// An agent herdr has not detected — one it doesn't know, or the first
// seconds after a start — gets the text typed, as tmux would type it.
func (h Herdr) Prompt(ws Workspace, pane Pane, text string) error {
	_, err := h.call("agent.prompt", map[string]any{"target": pane.ID, "text": text})
	var herr *HerdrError
	if errors.As(err, &herr) && herr.Code == "agent_not_found" {
		return h.Run(ws, pane, text)
	}
	return err
}

// States maps herdr's words onto this package's: the same four, and
// unknown (no agent detected in the workspace) is "".
func (h Herdr) States() (map[string]string, error) {
	list, err := h.list()
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	for _, w := range list {
		switch w.Status {
		case Working, Blocked, Done, Idle:
			states[w.Label] = w.Status
		default:
			states[w.Label] = ""
		}
	}
	return states, nil
}

// Select focuses the workspace; every client attached to the session
// follows, which is why Focus has nothing left to do.
func (h Herdr) Select(ws Workspace) error {
	_, err := h.call("workspace.focus", map[string]any{"workspace_id": ws.ID})
	return err
}

func (Herdr) Focus() error { return nil }

func (h Herdr) Close(ws Workspace) error {
	_, err := h.call("workspace.close", map[string]any{"workspace_id": ws.ID})
	return err
}

// Current is the workspace of the pane this process runs in, which
// herdr names in the environment.
func (h Herdr) Current() (Workspace, bool) {
	id := os.Getenv("HERDR_WORKSPACE_ID")
	if id == "" {
		return Workspace{}, false
	}
	list, err := h.list()
	if err != nil {
		return Workspace{}, false
	}
	for _, w := range list {
		if w.ID == id {
			return Workspace{ID: w.ID, Name: w.Label, Selected: w.Focused}, true
		}
	}
	return Workspace{}, false
}

func (Herdr) Describe(ws Workspace) string { return ws.Name }

func (h Herdr) AttachHint() string {
	if h.Inside() {
		return ""
	}
	if s := h.Session(); s != "default" {
		return "herdr --session " + s
	}
	return "herdr"
}

func (h Herdr) Notify(title, text string) {
	_, _ = h.call("notification.show", map[string]any{"title": title, "body": text})
}

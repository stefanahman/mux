// Package muxtest holds test doubles for the multiplexers: a herdr
// server on a socket, a cmux CLI, a private tmux server. Consumers of
// package mux test their flows against them the way mux does.
package muxtest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// FakeHerdr is a herdr server good enough for the driver: the methods
// it calls, over the same newline-JSON socket, with the shapes seen on
// the wire of herdr 0.9.
type FakeHerdr struct {
	t      testing.TB
	socket string

	mu         sync.Mutex
	seq        int
	workspaces []*fakeHerdrWorkspace
	focused    string            // workspace id
	statuses   map[string]string // workspace label → agent_status
	foreground map[string]string // pane id → foreground process name
	agents     map[string]string // pane id → detected agent; agent.prompt needs one
	blocked    map[string]bool   // pane id → agent.prompt refuses
	typed      map[string][]string
	notes      []string
}

type fakeHerdrWorkspace struct {
	id, label, cwd string
	panes          []string // in creation order, the root first
	tabs           []string // tab labels, the first is "1"
	paneCwd        map[string]string
	paneTab        map[string]string // pane id → tab id
	focus          bool
}

// HerdrWorkspace is a snapshot of a fake workspace.
type HerdrWorkspace struct {
	ID, Label, Cwd string
	Panes          []string // in creation order, the root first
	Tabs           []string
	Focus          bool // created with focus
}

// Pane is the root pane.
func (w HerdrWorkspace) Pane() string { return w.Panes[0] }

type herdrError struct{ code, message string }

// NewFakeHerdr starts a server on a socket of its own.
func NewFakeHerdr(t testing.TB) *FakeHerdr {
	t.Helper()
	dir, err := os.MkdirTemp("", "hd") // socket paths are short-lived and length-limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &FakeHerdr{
		t:          t,
		socket:     filepath.Join(dir, "herdr.sock"),
		statuses:   map[string]string{},
		foreground: map[string]string{},
		agents:     map[string]string{},
		blocked:    map[string]bool{},
		typed:      map[string][]string{},
	}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

// Socket is where the server listens.
func (f *FakeHerdr) Socket() string { return f.socket }

func (f *FakeHerdr) serve(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)
	for {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(line, &req) == nil {
				result, herr := f.dispatch(req.Method, req.Params)
				var resp map[string]any
				if herr != nil {
					resp = map[string]any{"id": req.ID, "error": map[string]string{"code": herr.code, "message": herr.message}}
				} else {
					resp = map[string]any{"id": req.ID, "result": result}
				}
				b, _ := json.Marshal(resp)
				_, _ = c.Write(append(b, '\n'))
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *FakeHerdr) find(id string) *fakeHerdrWorkspace {
	for _, w := range f.workspaces {
		if w.id == id {
			return w
		}
	}
	return nil
}

func (f *FakeHerdr) paneOf(paneID string) *fakeHerdrWorkspace {
	for _, w := range f.workspaces {
		for _, p := range w.panes {
			if p == paneID {
				return w
			}
		}
	}
	return nil
}

func (f *FakeHerdr) addPane(w *fakeHerdrWorkspace, tab, cwd string) string {
	id := fmt.Sprintf("%s:p%d", w.id, len(w.panes)+1)
	w.panes = append(w.panes, id)
	if cwd == "" {
		cwd = w.cwd
	}
	w.paneCwd[id] = cwd
	w.paneTab[id] = tab
	return id
}

// SplitPane adds a pane to a workspace, the way a user's split would.
func (f *FakeHerdr) SplitPane(label string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.workspaces {
		if w.label == label {
			return f.addPane(w, w.id+":t1", "")
		}
	}
	f.t.Fatalf("no workspace %q", label)
	return ""
}

func (f *FakeHerdr) info(w *fakeHerdrWorkspace) map[string]any {
	status := f.statuses[w.label]
	if status == "" {
		status = "unknown"
	}
	return map[string]any{"workspace_id": w.id, "label": w.label, "agent_status": status, "focused": w.id == f.focused, "active_tab_id": w.id + ":t1", "tab_count": len(w.tabs), "pane_count": len(w.panes)}
}

func (f *FakeHerdr) paneInfo(w *fakeHerdrWorkspace, id string) map[string]any {
	var agent any
	if a := f.agents[id]; a != "" {
		agent = a
	}
	return map[string]any{"pane_id": id, "workspace_id": w.id, "tab_id": w.paneTab[id], "cwd": w.paneCwd[id], "agent": agent, "agent_status": f.info(w)["agent_status"]}
}

func (f *FakeHerdr) dispatch(method string, raw json.RawMessage) (any, *herdrError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p struct {
		Cwd, Label, WorkspaceID, PaneID, TargetPaneID, Target, Text, Title, Body, Direction string
		Focus                                                                               bool
		Keys                                                                                []string
	}
	_ = json.Unmarshal(raw, &struct {
		Cwd          *string   `json:"cwd"`
		Label        *string   `json:"label"`
		WorkspaceID  *string   `json:"workspace_id"`
		PaneID       *string   `json:"pane_id"`
		TargetPaneID *string   `json:"target_pane_id"`
		Target       *string   `json:"target"`
		Text         *string   `json:"text"`
		Title        *string   `json:"title"`
		Body         *string   `json:"body"`
		Direction    *string   `json:"direction"`
		Focus        *bool     `json:"focus"`
		Keys         *[]string `json:"keys"`
	}{&p.Cwd, &p.Label, &p.WorkspaceID, &p.PaneID, &p.TargetPaneID, &p.Target, &p.Text, &p.Title, &p.Body, &p.Direction, &p.Focus, &p.Keys})
	switch method {
	case "ping":
		return map[string]any{"type": "pong"}, nil
	case "workspace.list":
		list := []map[string]any{}
		for _, w := range f.workspaces {
			list = append(list, f.info(w))
		}
		return map[string]any{"type": "workspace_list", "workspaces": list}, nil
	case "workspace.create":
		f.seq++
		w := &fakeHerdrWorkspace{id: fmt.Sprintf("w%d", f.seq), label: p.Label, cwd: p.Cwd, focus: p.Focus, tabs: []string{"1"}, paneCwd: map[string]string{}, paneTab: map[string]string{}}
		f.addPane(w, w.id+":t1", p.Cwd)
		f.workspaces = append(f.workspaces, w)
		if p.Focus || f.focused == "" {
			f.focused = w.id
		}
		return map[string]any{"type": "workspace_created", "workspace": f.info(w), "root_pane": f.paneInfo(w, w.panes[0])}, nil
	case "workspace.focus":
		w := f.find(p.WorkspaceID)
		if w == nil {
			return nil, &herdrError{"not_found", "workspace not found"}
		}
		f.focused = w.id
		return map[string]any{"type": "workspace_info", "workspace": f.info(w)}, nil
	case "workspace.close":
		for i, w := range f.workspaces {
			if w.id == p.WorkspaceID {
				f.workspaces = append(f.workspaces[:i], f.workspaces[i+1:]...)
				return map[string]any{"type": "ok"}, nil
			}
		}
		return nil, &herdrError{"not_found", "workspace not found"}
	case "tab.create":
		w := f.find(p.WorkspaceID)
		if w == nil {
			return nil, &herdrError{"not_found", "workspace not found"}
		}
		w.tabs = append(w.tabs, p.Label)
		tab := fmt.Sprintf("%s:t%d", w.id, len(w.tabs))
		id := f.addPane(w, tab, p.Cwd)
		return map[string]any{"type": "tab_created", "tab": map[string]any{"tab_id": tab, "label": p.Label}, "root_pane": f.paneInfo(w, id)}, nil
	case "tab.list":
		w := f.find(p.WorkspaceID)
		if w == nil {
			return nil, &herdrError{"not_found", "workspace not found"}
		}
		tabs := []map[string]any{}
		for i, l := range w.tabs {
			tabs = append(tabs, map[string]any{"tab_id": fmt.Sprintf("%s:t%d", w.id, i+1), "label": l})
		}
		return map[string]any{"type": "tab_list", "tabs": tabs}, nil
	case "pane.split":
		// herdr splits the focused pane unless target_pane_id names one;
		// pane_id is not that parameter and is ignored, as herdr does.
		w := f.paneOf(p.TargetPaneID)
		target := p.TargetPaneID
		if w == nil {
			if w = f.find(f.focused); w == nil {
				return nil, &herdrError{"not_found", "no focused pane"}
			}
			target = w.panes[0]
		}
		id := f.addPane(w, w.paneTab[target], p.Cwd)
		return map[string]any{"type": "pane_info", "pane": f.paneInfo(w, id)}, nil
	case "pane.close":
		w := f.paneOf(p.PaneID)
		if w == nil {
			return nil, &herdrError{"not_found", "pane not found"}
		}
		for i, id := range w.panes {
			if id == p.PaneID {
				w.panes = append(w.panes[:i], w.panes[i+1:]...)
			}
		}
		return map[string]any{"type": "ok"}, nil
	case "pane.list":
		panes := []map[string]any{}
		for _, w := range f.workspaces {
			if p.WorkspaceID == "" || w.id == p.WorkspaceID {
				for _, id := range w.panes {
					panes = append(panes, f.paneInfo(w, id))
				}
			}
		}
		return map[string]any{"type": "pane_list", "panes": panes}, nil
	case "pane.process_info":
		if f.paneOf(p.PaneID) == nil {
			return nil, &herdrError{"not_found", "pane not found"}
		}
		name := f.foreground[p.PaneID]
		if name == "" {
			name = "zsh"
		}
		return map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": p.PaneID, "foreground_processes": []map[string]any{{"name": name, "pid": 1}}}}, nil
	case "pane.send_input":
		if f.paneOf(p.PaneID) == nil {
			return nil, &herdrError{"not_found", "pane not found"}
		}
		line := p.Text
		for _, k := range p.Keys {
			line += "<" + k + ">"
		}
		f.typed[p.PaneID] = append(f.typed[p.PaneID], line)
		return map[string]any{"type": "ok"}, nil
	case "agent.prompt":
		if f.paneOf(p.Target) == nil || f.agents[p.Target] == "" {
			return nil, &herdrError{"agent_not_found", "agent target " + p.Target + " not found"}
		}
		if f.blocked[p.Target] {
			return nil, &herdrError{"agent_blocked", "agent " + p.Target + " is blocked and requires interactive input"}
		}
		f.typed[p.Target] = append(f.typed[p.Target], "prompt:"+p.Text)
		return map[string]any{"type": "agent_prompted"}, nil
	case "notification.show":
		f.notes = append(f.notes, p.Title+": "+p.Body)
		return map[string]any{"type": "notification_shown", "shown": true}, nil
	}
	return nil, &herdrError{"method_not_found", "unknown method " + method}
}

// Workspace is a snapshot of the workspace with the label, nil when
// there is none.
func (f *FakeHerdr) Workspace(label string) *HerdrWorkspace {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.workspaces {
		if w.label == label {
			return &HerdrWorkspace{ID: w.id, Label: w.label, Cwd: w.cwd, Panes: append([]string(nil), w.panes...), Tabs: append([]string(nil), w.tabs...), Focus: w.focus}
		}
	}
	return nil
}

// PaneCwd is the directory a pane was created in.
func (f *FakeHerdr) PaneCwd(pane string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w := f.paneOf(pane); w != nil {
		return w.paneCwd[pane]
	}
	return ""
}

// Focused is the id of the focused workspace.
func (f *FakeHerdr) Focused() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.focused
}

// Typed is what reached a pane: lines with their keys as <enter>, and
// prompts as prompt:<text>.
func (f *FakeHerdr) Typed(pane string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.typed[pane]...)
}

// Notes are the notifications shown, as "title: body".
func (f *FakeHerdr) Notes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notes...)
}

// SetStatus sets a workspace's agent_status.
func (f *FakeHerdr) SetStatus(label, status string) { f.set(f.statuses, label, status) }

// SetForeground sets a pane's foreground process name.
func (f *FakeHerdr) SetForeground(pane, name string) { f.set(f.foreground, pane, name) }

// SetAgent makes herdr detect an agent in the pane.
func (f *FakeHerdr) SetAgent(pane, agent string) { f.set(f.agents, pane, agent) }

// SetBlocked makes agent.prompt refuse for the pane.
func (f *FakeHerdr) SetBlocked(pane string, blocked bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked[pane] = blocked
}

func (f *FakeHerdr) set(m map[string]string, k, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m[k] = v
}

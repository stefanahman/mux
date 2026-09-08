package mux_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stefanahman/mux"
	"github.com/stefanahman/mux/muxtest"
)

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "cmux" {
		os.Exit(muxtest.FakeCmuxMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestDetect(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	t.Setenv("CMUX_WORKSPACE_ID", "")
	tm, hd, cm := mux.Tmux{SessionName: "s"}, mux.NewHerdr("/x/herdr.sock"), mux.Cmux{}
	if d := mux.Detect(tm, hd, cm); d.Kind() != "tmux" {
		t.Errorf("outside everything: %s, want tmux", d.Kind())
	}
	t.Setenv("HERDR_ENV", "1")
	if d := mux.Detect(tm, hd, cm); d.Kind() != "herdr" {
		t.Errorf("inside herdr: %s", d.Kind())
	}
	t.Setenv("HERDR_ENV", "")
	t.Setenv("CMUX_WORKSPACE_ID", "W")
	if d := mux.Detect(tm, hd, cm); d.Kind() != "cmux" {
		t.Errorf("inside cmux: %s", d.Kind())
	}
	for _, k := range []string{"tmux", "herdr", "cmux"} {
		if d := mux.ByKind(k); d == nil || d.Kind() != k {
			t.Errorf("ByKind(%s) = %v", k, d)
		}
	}
	if mux.ByKind("screen") != nil {
		t.Error("ByKind should not know screen")
	}
	for name, shell := range map[string]bool{"zsh": true, "-bash": true, "/bin/sh": true, "nu": true, "claude": false, "nvim": false, "2.1.263": false} {
		if mux.IsShell(name) != shell {
			t.Errorf("IsShell(%q) = %v", name, !shell)
		}
	}
}

func TestTmux(t *testing.T) {
	muxtest.StartTmux(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := mux.Tmux{SessionName: "reviews", Keepalive: "keep"}
	if err := d.Ping(); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second time the session exists
		if err := d.Prepare(root); err != nil {
			t.Fatal(err)
		}
	}
	if ws, err := d.Workspaces(); err != nil || len(ws) != 0 {
		t.Fatalf("fresh session: %v, %v (the keepalive window is not a workspace)", ws, err)
	}
	dir := filepath.Join(root, "pr-1")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := d.Create("pr-1-fix", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ws.ID, "@") || ws.Name != "pr-1-fix" {
		t.Errorf("created %+v", ws)
	}
	list, _ := d.Workspaces()
	if len(list) != 1 || list[0].Name != "pr-1-fix" || list[0].Cwd != dir {
		t.Errorf("Workspaces() = %+v", list)
	}
	pane, err := d.AgentPane(ws)
	if err != nil || !strings.HasPrefix(pane.ID, "%") {
		t.Fatalf("AgentPane = %+v, %v", pane, err)
	}
	if !d.AtShell(ws, pane) {
		ps, _ := d.Processes(ws, pane)
		t.Errorf("a new window is at its shell; processes %v", ps)
	}
	if err := d.Run(ws, pane, "sleep 30"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !d.AtShell(ws, pane) }, "sleep to take the pane")
	if _, err := d.AddTab(ws, "x", ""); !errors.Is(err, mux.ErrUnsupported) {
		t.Errorf("AddTab on tmux: %v, want ErrUnsupported", err)
	}
	second, err := d.Split(ws, pane, mux.Right, root)
	if err != nil {
		t.Fatal(err)
	}
	if panes, _ := d.Panes(ws); len(panes) != 2 || panes[0] != pane || panes[1] != second {
		t.Errorf("Panes() = %v, want the root then the split", panes)
	}
	if tabs, err := d.Layout(ws); err != nil || !reflect.DeepEqual(tabs, []mux.Tab{{Name: "pr-1-fix", Panes: []mux.Pane{pane, second}}}) {
		t.Errorf("Layout() = %v, %v", tabs, err)
	}
	if out, _ := exec.Command("tmux", "set-option", "-w", "-t", ws.ID, mux.ClaudeStateOption, "working").CombinedOutput(); len(out) > 0 {
		t.Fatalf("set-option: %s", out)
	}
	if states, err := d.States(); err != nil || states["pr-1-fix"] != mux.Working || len(states) != 1 {
		t.Errorf("States() = %v, %v", states, err)
	}
	if err := d.Select(ws); err != nil {
		t.Fatal(err)
	}
	// A server started from inside a Claude Code session carries its
	// markers in the global environment; Ping and Prepare say so.
	if _, err := exec.Command("tmux", "set-environment", "-g", "CLAUDECODE", "1").Output(); err != nil {
		t.Fatal(err)
	}
	if err := d.Ping(); err == nil || !strings.Contains(err.Error(), "child sessions") || !strings.Contains(err.Error(), "CLAUDECODE") {
		t.Errorf("Ping with a tainted server: %v", err)
	}
	if err := d.Prepare(root); err == nil {
		t.Error("Prepare should refuse a tainted server")
	}
	if _, err := exec.Command("tmux", "set-environment", "-gu", "CLAUDECODE").Output(); err != nil {
		t.Fatal(err)
	}
	if err := d.Ping(); err != nil {
		t.Errorf("Ping after the marker is gone: %v", err)
	}
	if got := d.Describe(ws); got != "reviews:pr-1-fix" {
		t.Errorf("Describe = %q", got)
	}
	if hint := d.AttachHint(); hint != "tmux attach -t reviews" {
		t.Errorf("AttachHint = %q", hint)
	}
	if _, ok := d.Current(); ok {
		t.Error("Current outside tmux")
	}
	if d.Session() != "reviews" || d.Kind() != "tmux" || d.Inside() {
		t.Error("Session, Kind or Inside")
	}
	if err := d.Close(ws); err != nil {
		t.Fatal(err)
	}
	if list, _ := d.Workspaces(); len(list) != 0 {
		t.Errorf("after Close: %+v", list)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for range 50 {
		if cond() {
			return
		}
		exec.Command("sleep", "0.1").Run()
	}
	t.Fatalf("waited for %s", what)
}

// Prepare's server start drops the caller's Claude Code markers, so
// a server owl or spaces starts from inside a session is clean.
func TestTmuxPrepareStartsClean(t *testing.T) {
	dir, err := os.MkdirTemp("", "mux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("TMUX", "")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	tm := mux.Tmux{}
	if err := tm.Prepare(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-server").Run() })
	out, _ := exec.Command("tmux", "show-environment", "-g").Output()
	if strings.Contains(string(out), "CLAUDECODE") {
		t.Errorf("the server inherited the markers:\n%s", out)
	}
	if err := tm.Ping(); err != nil {
		t.Errorf("the server Prepare started: %v", err)
	}
}

func TestHerdr(t *testing.T) {
	fake := muxtest.NewFakeHerdr(t)
	for _, v := range []string{"HERDR_ENV", "HERDR_WORKSPACE_ID", "HERDR_SESSION"} {
		t.Setenv(v, "")
	}
	d := mux.NewHerdr(fake.Socket())
	if err := d.Prepare("/repo"); err != nil {
		t.Fatal(err)
	}
	other, _ := d.Create("other", "/elsewhere") // the focused one, as the first created is
	ws, err := d.Create("pr-42-fix", "/repo/wt")
	if err != nil {
		t.Fatal(err)
	}
	if fw := fake.Workspace("pr-42-fix"); fw == nil || fw.Cwd != "/repo/wt" || fw.Focus {
		t.Fatalf("created %+v, want cwd /repo/wt without focus", fw)
	}
	if list, _ := d.Workspaces(); len(list) != 2 || list[1].Name != "pr-42-fix" || list[1].ID != ws.ID {
		t.Errorf("Workspaces() = %+v", list)
	}
	root, _ := d.AgentPane(ws)
	if root.ID != fake.Workspace("pr-42-fix").Pane() {
		t.Errorf("AgentPane = %v, want the root", root)
	}
	if err := d.Run(ws, root, "claude x"); err != nil {
		t.Fatal(err)
	}
	if got := fake.Typed(root.ID); !reflect.DeepEqual(got, []string{"claude x<enter>"}) {
		t.Errorf("typed %v", got)
	}
	if !d.AtShell(ws, root) {
		t.Error("a fresh pane runs zsh: at shell")
	}
	fake.SetForeground(root.ID, "claude")
	if d.AtShell(ws, root) {
		t.Error("claude in the foreground: not at shell")
	}
	// No agent detected yet: the prompt is typed. Detected: agent.prompt.
	if err := d.Prompt(ws, root, "early"); err != nil {
		t.Fatal(err)
	}
	fake.SetAgent(root.ID, "claude")
	if err := d.Prompt(ws, root, "look"); err != nil {
		t.Fatal(err)
	}
	if got := fake.Typed(root.ID); got[len(got)-2] != "early<enter>" || got[len(got)-1] != "prompt:look" {
		t.Errorf("typed %v", got)
	}
	fake.SetBlocked(root.ID, true)
	var herr *mux.HerdrError
	if err := d.Prompt(ws, root, "x"); !errors.As(err, &herr) || herr.Code != "agent_blocked" {
		t.Errorf("prompt while blocked: %v", err)
	}
	// The agent's pane is the one herdr detects an agent in, not the root.
	split := fake.SplitPane("pr-42-fix")
	fake.SetAgent(root.ID, "")
	fake.SetAgent(split, "claude")
	if p, _ := d.AgentPane(ws); p.ID != split {
		t.Errorf("AgentPane = %v, want %s where the agent is", p, split)
	}
	fake.SetStatus("pr-42-fix", "working")
	fake.SetStatus("other", "nonsense")
	if states, err := d.States(); err != nil || states["pr-42-fix"] != mux.Working || states["other"] != "" {
		t.Errorf("States() = %v, %v", states, err)
	}
	tab, err := d.AddTab(ws, "nvim", "/repo/branches")
	if err != nil || fake.PaneCwd(tab.ID) != "/repo/branches" || !reflect.DeepEqual(fake.Workspace("pr-42-fix").Tabs, []string{"1", "nvim"}) {
		t.Errorf("AddTab = %v, %v; cwd %q; tabs %v", tab, err, fake.PaneCwd(tab.ID), fake.Workspace("pr-42-fix").Tabs)
	}
	// Split names its pane the way herdr's schema wants; the fake, like
	// herdr, would otherwise split the focused workspace's pane.
	right, err := d.Split(ws, root, mux.Right, "/repo/right")
	if err != nil {
		t.Fatal(err)
	}
	if fw := fake.Workspace("pr-42-fix"); !contains(fw.Panes, right.ID) || fake.PaneCwd(right.ID) != "/repo/right" {
		t.Errorf("Split put %s elsewhere: panes %v (focused workspace is %s)", right.ID, fw.Panes, fake.Focused())
	}
	if tabs, err := d.Layout(ws); err != nil || !reflect.DeepEqual(tabs, []mux.Tab{{Name: "1", Panes: []mux.Pane{root, {ID: split}, right}}, {Name: "nvim", Panes: []mux.Pane{tab}}}) {
		t.Errorf("Layout() = %v, %v", tabs, err)
	}
	if err := d.Select(ws); err != nil || fake.Focused() != ws.ID {
		t.Errorf("Select: %v, focused %s", err, fake.Focused())
	}
	if _, ok := d.Current(); ok {
		t.Error("Current outside herdr")
	}
	t.Setenv("HERDR_WORKSPACE_ID", ws.ID)
	if cur, ok := d.Current(); !ok || cur.Name != "pr-42-fix" {
		t.Errorf("Current = %+v, %v", cur, ok)
	}
	d.Notify("pr-owl", "done")
	if notes := fake.Notes(); !reflect.DeepEqual(notes, []string{"pr-owl: done"}) {
		t.Errorf("notes %v", notes)
	}
	if d.Session() != "default" || d.AttachHint() != "herdr" {
		t.Errorf("Session %q, AttachHint %q", d.Session(), d.AttachHint())
	}
	t.Setenv("HERDR_SESSION", "work")
	if d.Session() != "work" || d.AttachHint() != "herdr --session work" {
		t.Errorf("Session %q, AttachHint %q", d.Session(), d.AttachHint())
	}
	t.Setenv("HERDR_ENV", "1")
	if !d.Inside() || d.AttachHint() != "" {
		t.Error("inside herdr")
	}
	if err := d.Close(ws); err != nil || fake.Workspace("pr-42-fix") != nil {
		t.Errorf("Close: %v", err)
	}
	if err := d.Close(other); err != nil {
		t.Error(err)
	}
	if s := mux.NewHerdr("~/.config/herdr/sessions/work/herdr.sock"); !strings.HasSuffix(s.Socket, "/sessions/work/herdr.sock") || strings.HasPrefix(s.Socket, "~") {
		t.Errorf("socket %q: ~ not expanded", s.Socket)
	}
	t.Setenv("HERDR_SESSION", "")
	if s := mux.NewHerdr("/x/sessions/review/herdr.sock"); s.Session() != "review" {
		t.Errorf("session from the socket path = %q", s.Session())
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestHerdrUnreachable(t *testing.T) {
	d := mux.NewHerdr("/nonexistent/herdr.sock")
	if err := d.Ping(); err == nil || !strings.Contains(err.Error(), "herdr") {
		t.Errorf("Ping: %v", err)
	}
}

func TestCmux(t *testing.T) {
	fake := muxtest.InstallFakeCmux(t)
	fake.AddWorkspace("", "HOME")
	d := mux.NewCmux()
	if err := d.Prepare("/repo"); err != nil {
		t.Fatal(err)
	}
	ws, err := d.Create("pr-42-fix", "/repo/wt")
	if err != nil {
		t.Fatal(err)
	}
	if ws.ID != "WS-2" || ws.Name != "pr-42-fix" || ws.Cwd != "/repo/wt" {
		t.Errorf("created %+v", ws)
	}
	if !contains(fake.Calls(), "workspace create --name pr-42-fix --cwd /repo/wt --focus false") {
		t.Errorf("create call missing from %v", fake.Calls())
	}
	if list, _ := d.Workspaces(); len(list) != 2 || list[0].Name != "~" || list[1] != ws {
		t.Errorf("Workspaces() = %+v", list)
	}
	fw, _ := fake.Workspace("pr-42-fix")
	pane, err := d.AgentPane(ws)
	if err != nil || pane.ID != fw.Panes[0].Surfaces[0].ID {
		t.Fatalf("AgentPane = %v, %v; want the surface the workspace was created with", pane, err)
	}
	// A shell line and a prompt are both typed as they are.
	if err := d.Run(ws, pane, "claude x"); err != nil {
		t.Fatal(err)
	}
	if err := d.Prompt(ws, pane, "look"); err != nil {
		t.Fatal(err)
	}
	if got := fake.Typed(pane.ID); !reflect.DeepEqual(got, []string{"claude x", "<enter>", "look", "<enter>"}) {
		t.Errorf("typed %v", got)
	}

	tab, err := d.AddTab(ws, "nvim", "/repo/branches")
	if err != nil {
		t.Fatal(err)
	}
	fw, _ = fake.Workspace("pr-42-fix")
	if len(fw.Panes[0].Surfaces) != 2 || fw.Panes[0].Surfaces[1].ID != tab.ID || fw.Panes[0].Surfaces[1].Title != "nvim" {
		t.Errorf("AddTab: panes %+v", fw.Panes)
	}
	if got := fake.Typed(tab.ID); !reflect.DeepEqual(got, []string{"cd '/repo/branches'", "<enter>"}) {
		t.Errorf("the tab's shell was told to move: %v", got)
	}
	right, err := d.Split(ws, pane, mux.Right, "")
	if err != nil {
		t.Fatal(err)
	}
	fw, _ = fake.Workspace("pr-42-fix")
	if len(fw.Panes) != 2 || fw.Panes[1].Surfaces[0].ID != right.ID || len(fake.Typed(right.ID)) != 0 {
		t.Errorf("Split: panes %+v, typed %v", fw.Panes, fake.Typed(right.ID))
	}
	if panes, _ := d.Panes(ws); !reflect.DeepEqual(panes, []mux.Pane{pane, tab, right}) {
		t.Errorf("Panes() = %v", panes)
	}
	if tabs, err := d.Layout(ws); err != nil || !reflect.DeepEqual(tabs, []mux.Tab{{Name: "pr-42-fix", Panes: []mux.Pane{pane, right}}, {Name: "nvim", Panes: []mux.Pane{tab}}}) {
		t.Errorf("Layout() = %v, %v", tabs, err)
	}

	// The surface a workspace was created with has a tty: ps says what
	// runs in its foreground. A tab made through the API has none, and
	// its title does — the program, or the directory at a prompt.
	rootTTY := fw.Panes[0].Surfaces[0].TTY
	for _, c := range []struct {
		fg      []string
		atShell bool
	}{
		{nil, true},
		{[]string{"-/bin/zsh"}, true},
		{[]string{"-/bin/zsh", "/Users/x/.local/bin/claude", "node"}, false},
		{[]string{"nvim"}, false},
	} {
		fake.SetForeground(rootTTY, c.fg...)
		d := mux.NewCmux() // a run of its own: the snapshot is per run
		if got := d.AtShell(ws, pane); got != c.atShell {
			t.Errorf("AtShell with %v in the foreground = %v", c.fg, got)
		}
	}
	for _, c := range []struct {
		title    string
		atShell  bool
		programs []string
	}{
		{"Terminal", true, nil},
		{"~/Development/eden", true, nil},
		{"…/Development/bardo/bardo-system", true, nil},
		{"/tmp", true, nil},
		{"nvim", false, []string{"nvim"}},
		{"cd /Users/x/src && nvim", false, []string{"cd", "src", "&&", "nvim"}},
		{"✳ Claude Code", false, []string{"✳", "Claude", "Code"}},
	} {
		fake.SetTitle(tab.ID, c.title)
		d := mux.NewCmux()
		if got := d.AtShell(ws, tab); got != c.atShell {
			t.Errorf("AtShell on a tab titled %q = %v", c.title, got)
		}
		if got, _ := d.Processes(ws, tab); !reflect.DeepEqual(got, c.programs) {
			t.Errorf("Processes on a tab titled %q = %v, want %v", c.title, got, c.programs)
		}
	}
	if ps, _ := d.Processes(ws, right); len(ps) != 0 {
		t.Errorf("a fresh split is at its prompt: %v", ps)
	}

	// Reads come from a snapshot: a run over every workspace costs the
	// three commands, not three per workspace; a change refreshes it.
	before := len(fake.Calls())
	for _, w := range func() []mux.Workspace { l, _ := d.Workspaces(); return l }() {
		if _, err := d.Layout(w); err != nil {
			t.Fatal(err)
		}
		d.AtShell(w, mux.Pane{ID: fw.Panes[0].Surfaces[0].ID})
	}
	if n := len(fake.Calls()) - before; n > 3 {
		t.Errorf("reading two workspaces took %d commands, want at most 3", n)
	}
	fake.AddWorkspace("pr-9-late", "W9")
	if l, _ := d.Workspaces(); len(l) != 2 {
		t.Errorf("a snapshot is kept until the driver changes something: %d workspaces, want the 2 it knew", len(l))
	}
	if err := d.Run(ws, pane, "true"); err != nil {
		t.Fatal(err)
	}
	if l, _ := d.Workspaces(); len(l) != 3 {
		t.Errorf("after a change the snapshot is fresh: %d workspaces, want 3", len(l))
	}
	if err := d.Close(mux.Workspace{ID: "W9", Name: "pr-9-late"}); err != nil {
		t.Fatal(err)
	}

	for i, name := range []string{"pr-1-working", "pr-2-blocked", "pr-3-done", "pr-4-idle", "pr-5-stale", "pr-6-none"} {
		fake.AddWorkspace(name, "W"+string(rune('1'+i)))
	}
	fake.AddSession("W1", "running", true, "2026-09-08T16:00:00Z")
	fake.AddSession("W1", "idle", false, "2026-09-08T15:00:00Z") // an older, exited session must not shadow the live one
	fake.AddSession("W2", "needsInput", true, "2026-09-08T16:00:00Z")
	fake.AddSession("W3", "idle", true, "2026-09-08T16:00:00Z")
	fake.AddSession("W4", "idle", true, "2026-09-08T16:00:00Z")
	fake.AddSession("W5", "needsInput", false, "2026-09-08T16:00:00Z") // the agent exited; cmux keeps the record
	fake.AddNote("W3", false)
	fake.AddNote("W4", true)
	want := map[string]string{"~": "", "pr-42-fix": "", "pr-1-working": mux.Working, "pr-2-blocked": mux.Blocked, "pr-3-done": mux.Done, "pr-4-idle": mux.Idle, "pr-5-stale": "", "pr-6-none": ""}
	if got, err := d.States(); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("States() = %v, %v; want %v", got, err, want)
	}

	fake.AddNote(ws.ID, false)
	if err := d.Select(ws); err != nil {
		t.Fatal(err)
	}
	st := fake.State()
	if st.Selected != ws.ID || st.Notifications[len(st.Notifications)-1].Read {
		t.Errorf("after Select: selected %q, last note read %v (Select alone leaves notifications)", st.Selected, st.Notifications[len(st.Notifications)-1].Read)
	}
	if err := d.Seen(ws); err != nil {
		t.Fatal(err)
	}
	if st := fake.State(); !st.Notifications[len(st.Notifications)-1].Read {
		t.Error("after Seen the notification is read")
	}
	if d.AttachHint() != "cmux" || d.Inside() || d.Session() != "" || d.Describe(ws) != "pr-42-fix" {
		t.Error("outside cmux: AttachHint, Inside, Session or Describe")
	}
	if err := d.Focus(); err != nil || fake.State().Focused != "window:1" {
		t.Errorf("Focus outside cmux: %v, focused %q", err, fake.State().Focused)
	}
	if _, ok := d.Current(); ok {
		t.Error("Current outside cmux")
	}
	t.Setenv("CMUX_WORKSPACE_ID", ws.ID)
	if cur, ok := d.Current(); !ok || cur.Name != "pr-42-fix" || d.AttachHint() != "" || !d.Inside() {
		t.Errorf("inside the workspace: Current = %+v, %v", cur, ok)
	}
	if err := d.Focus(); err != nil {
		t.Errorf("Focus inside cmux: %v", err)
	}
	d.Notify("pr-owl", "all done")
	calls := fake.Calls()
	if got := calls[len(calls)-1]; got != "notify --title pr-owl --body all done --workspace WS-2" {
		t.Errorf("notify call %q", got)
	}
	if err := d.Close(ws); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Workspace("pr-42-fix"); ok {
		t.Error("workspace still there after Close")
	}
}

func TestCmuxUnreachable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cmux"), []byte("#!/bin/sh\necho 'cmux: socket not found' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CMUX_WORKSPACE_ID", "")
	err := mux.Cmux{}.Ping()
	if err == nil || !strings.Contains(err.Error(), "socket not found") || !strings.Contains(err.Error(), "CMUX_SOCKET_MODE=allowAll") {
		t.Errorf("outside cmux: %v", err)
	}
	t.Setenv("CMUX_WORKSPACE_ID", "W1")
	if err := (mux.Cmux{}).Ping(); err == nil || strings.Contains(err.Error(), "allowAll") {
		t.Errorf("inside cmux the hint makes no sense: %v", err)
	}
}

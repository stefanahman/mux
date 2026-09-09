package mux_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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

	// claude-status's pill comes before the hook store: cmux's record
	// says needsInput for the idle reminder too, so the pill decides
	// wherever the plugin wrote one, and the record only without it.
	fake.AddStatus("W1", "claude_code", "Needs input") // cmux's own pill: not ours
	fake.AddStatus("W2", "claude_code", "Needs input") // a value with a space, before ours
	fake.AddStatus("W2", "claude", "idle")             // the record says needsInput: the reminder
	fake.AddStatus("W3", "claude", "done")             // its note is unread
	fake.AddStatus("W4", "claude", "blocked")          // the record says idle
	fake.AddStatus("W5", "claude", "working")          // the record is stale, the pill is not
	fake.AddStatus("W6", "claude", "done")             // no record at all; its note read
	fake.AddNote("W6", true)
	want = map[string]string{"~": "", "pr-42-fix": "", "pr-1-working": mux.Working, "pr-2-blocked": mux.Idle, "pr-3-done": mux.Done, "pr-4-idle": mux.Blocked, "pr-5-stale": mux.Working, "pr-6-none": mux.Idle}
	reads := func() int {
		n := 0
		for _, c := range fake.Calls() {
			if strings.HasPrefix(c, "list-status ") {
				n++
			}
		}
		return n
	}
	p := mux.NewCmux()
	before = reads()
	if got, err := p.States(); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("States() with pills = %v, %v; want %v", got, err, want)
	}
	if n, workspaces := reads()-before, len(fake.State().Workspaces); n != workspaces {
		t.Errorf("reading the pills took %d list-status calls for %d workspaces", n, workspaces)
	}
	before = reads()
	if _, err := p.States(); err != nil {
		t.Fatal(err)
	}
	if reads() != before {
		t.Error("a second States() on the same run reads the pills again")
	}
	if err := p.Seen(mux.Workspace{ID: "W3", Name: "pr-3-done"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.States(); got["pr-3-done"] != mux.Idle || reads() == before {
		t.Errorf("after Seen the pill's done is idle: %v (pills re-read: %v)", got["pr-3-done"], reads() != before)
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

// TestCmuxGroups: a workspace joins a named sidebar group, the group
// is made once and added to afterwards, and the style it is painted
// with on creation never decides whether the grouping worked.
func TestCmuxGroups(t *testing.T) {
	fake := muxtest.InstallFakeCmux(t)
	fake.AddWorkspace("pr-1", "W1")
	fake.AddWorkspace("pr-2", "W2")
	fake.AddWorkspace("bar-3", "W3")
	style := mux.GroupStyle{Color: "#4C8DFF", Icon: "eye"}

	d := mux.NewCmux()
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W1", Name: "pr-1"}, style); err != nil {
		t.Fatal(err)
	}
	g, ok := fake.Group("reviews")
	if !ok || g.AnchorID != "W1" || !reflect.DeepEqual(g.MemberIDs, []string{"W1"}) {
		t.Fatalf("after the first call: %+v, %v", g, ok)
	}
	if g.Color != "#4C8DFF" || g.Icon != "eye" {
		t.Errorf("style not applied on creation: colour %q icon %q", g.Color, g.Icon)
	}

	// The second workspace joins that group; no second group is made.
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W2", Name: "pr-2"}, style); err != nil {
		t.Fatal(err)
	}
	if g, _ := fake.Group("reviews"); !reflect.DeepEqual(g.MemberIDs, []string{"W1", "W2"}) {
		t.Errorf("second call: members %v", g.MemberIDs)
	}
	if n := len(fake.Groups()); n != 1 {
		t.Errorf("%d groups, want the one", n)
	}
	// Joining an existing group does not repaint it.
	if n := countCalls(fake.Calls(), "workspace-group set-color"); n != 1 {
		t.Errorf("set-color ran %d times, want once — on creation only", n)
	}

	// A member again changes nothing: it costs the read that finds it
	// there, and no verb that writes.
	adds, creates := countCalls(fake.Calls(), "workspace-group add"), countCalls(fake.Calls(), "workspace-group create")
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W1", Name: "pr-1"}, style); err != nil {
		t.Fatal(err)
	}
	if g, _ := fake.Group("reviews"); len(g.MemberIDs) != 2 {
		t.Errorf("a member added twice: %v", g.MemberIDs)
	}
	if countCalls(fake.Calls(), "workspace-group add") != adds || countCalls(fake.Calls(), "workspace-group create") != creates {
		t.Errorf("a workspace already in the group was written to cmux: %v", fake.Calls())
	}

	// A different name is a different group, anchored on its own
	// workspace: the two lists do not share a container.
	if err := mux.Group(d, "features", mux.Workspace{ID: "W3", Name: "bar-3"}, mux.GroupStyle{}); err != nil {
		t.Fatal(err)
	}
	f, ok := fake.Group("features")
	if !ok || f.AnchorID != "W3" || len(fake.Groups()) != 2 {
		t.Fatalf("features: %+v, %v, %d groups", f, ok, len(fake.Groups()))
	}
	// An empty style paints nothing.
	if f.Color != "" || f.Icon != "" {
		t.Errorf("empty style painted: colour %q icon %q", f.Color, f.Icon)
	}
	if n := countCalls(fake.Calls(), "workspace-group set-icon"); n != 1 {
		t.Errorf("set-icon ran %d times, want once — the styled group only", n)
	}
}

// TestCmuxGroupReadsOnce: the group list is one read per run, like the
// workspace list, and a change to it is seen.
func TestCmuxGroupReadsOnce(t *testing.T) {
	fake := muxtest.InstallFakeCmux(t)
	fake.AddWorkspace("pr-1", "W1")
	fake.AddWorkspace("pr-2", "W2")
	fake.AddGroup("reviews", "W1")

	d := mux.NewCmux()
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W2"}, mux.GroupStyle{}); err != nil {
		t.Fatal(err)
	}
	// One list, one add: the second call sees the kept snapshot for the
	// membership it already knows.
	if n := countCalls(fake.Calls(), "workspace-group list"); n != 1 {
		t.Errorf("the group list was read %d times, want once", n)
	}
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W2"}, mux.GroupStyle{}); err != nil {
		t.Fatal(err)
	}
	// The add forgot the snapshot, so this call read again — and found
	// W2 a member, so it did nothing.
	if n := countCalls(fake.Calls(), "workspace-group list"); n != 2 {
		t.Errorf("after a change the list was read %d times, want twice", n)
	}
	if n := countCalls(fake.Calls(), "workspace-group add"); n != 1 {
		t.Errorf("add ran %d times, want once", n)
	}
}

// TestCmuxGroupStyleIsBestEffort: cmux refusing the icon leaves the
// workspace grouped and the caller none the wiser — an SF Symbol name
// comes from a config file, and a wrong one must not stop an open.
func TestCmuxGroupStyleIsBestEffort(t *testing.T) {
	fake := muxtest.InstallFakeCmux(t)
	fake.AddWorkspace("pr-1", "W1")
	fake.Reject("set-icon")

	d := mux.NewCmux()
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W1"}, mux.GroupStyle{Color: "#4C8DFF", Icon: "nonesuch"}); err != nil {
		t.Fatalf("a refused icon failed the grouping: %v", err)
	}
	g, ok := fake.Group("reviews")
	if !ok || !reflect.DeepEqual(g.MemberIDs, []string{"W1"}) {
		t.Fatalf("not grouped: %+v, %v", g, ok)
	}
	if g.Icon != "" {
		t.Errorf("the fake refused set-icon, yet the icon is %q", g.Icon)
	}
	if g.Color != "#4C8DFF" {
		t.Errorf("the colour before it should still be set: %q", g.Color)
	}

	// The grouping itself is the other half: when cmux refuses that,
	// the caller hears about it.
	fake.AddWorkspace("pr-2", "W2")
	fake.Reject("add")
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W2"}, mux.GroupStyle{}); err == nil {
		t.Error("a refused add reported success")
	}
	if g, _ := fake.Group("reviews"); len(g.MemberIDs) != 1 {
		t.Errorf("the refused workspace joined anyway: %v", g.MemberIDs)
	}
}

// TestGroupWithoutGrouper: tmux and herdr have no groups, so the
// package function is a no-op there rather than an error the caller
// has to know to ignore.
func TestGroupWithoutGrouper(t *testing.T) {
	for _, d := range []mux.Driver{mux.Tmux{SessionName: "reviews"}, mux.NewHerdr("/nonexistent/herdr.sock")} {
		if err := mux.Group(d, "reviews", mux.Workspace{ID: "W1", Name: "pr-1"}, mux.GroupStyle{Color: "#4C8DFF"}); err != nil {
			t.Errorf("%s: %v", d.Kind(), err)
		}
	}
}

// countCalls counts the fake's invocations naming verb. Not a prefix
// match: a read carries the driver's global flags in front of it
// (`--json --id-format both workspace-group list`).
func countCalls(calls []string, verb string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, verb) {
			n++
		}
	}
	return n
}

// TestCmuxGroupAdoptsTheAnchor: cmux 0.64.22 answers a create with a
// group of two — a workspace it generated to carry the header, and the
// one the caller named. The driver moves the anchor onto the caller's
// workspace and closes the generated one, so the sidebar shows the
// group on a workspace that means something and loses it when that
// workspace closes.
func TestCmuxGroupAdoptsTheAnchor(t *testing.T) {
	fake := muxtest.InstallFakeCmux(t)
	fake.AddWorkspace("pr-1", "W1")
	before := len(fake.State().Workspaces)

	d := mux.NewCmux()
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W1", Name: "pr-1"}, mux.GroupStyle{}); err != nil {
		t.Fatal(err)
	}
	g, ok := fake.Group("reviews")
	if !ok || g.AnchorID != "W1" || !reflect.DeepEqual(g.MemberIDs, []string{"W1"}) {
		t.Fatalf("group = %+v, %v; want anchored on W1 holding only it", g, ok)
	}
	if n := len(fake.State().Workspaces); n != before {
		t.Errorf("%d workspaces, want the %d there were: the generated anchor outlived the call", n, before)
	}
	if countCalls(fake.Calls(), "workspace-group set-anchor") != 1 || countCalls(fake.Calls(), "workspace close") != 1 {
		t.Errorf("adopting the anchor took: %v", fake.Calls())
	}

	// Closing the workspace takes the group with it — the reason for
	// all of the above.
	if err := d.Close(mux.Workspace{ID: "W1", Name: "pr-1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Group("reviews"); ok {
		t.Error("the group outlived its last member")
	}
}

// TestCmuxGroupLeavesAThirdWorkspaceAlone: closing a workspace cannot
// be undone, so the anchor is only adopted from the shape a fresh
// create leaves — the caller's workspace and one other. A group that
// comes back holding anything else keeps its generated anchor, and the
// call still succeeds.
func TestCmuxGroupLeavesAThirdWorkspaceAlone(t *testing.T) {
	fake := muxtest.InstallFakeCmux(t)
	fake.AddWorkspace("pr-1", "W1")
	fake.AddWorkspace("mine", "W9")
	fake.CaptureOnCreate("W9")
	before := len(fake.State().Workspaces)

	d := mux.NewCmux()
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W1", Name: "pr-1"}, mux.GroupStyle{}); err != nil {
		t.Fatal(err)
	}
	g, _ := fake.Group("reviews")
	if len(g.MemberIDs) != 3 || g.AnchorID == "W1" {
		t.Fatalf("group = %+v; want the three it came back with, anchor untouched", g)
	}
	if n := countCalls(fake.Calls(), "workspace close"); n != 0 {
		t.Errorf("closed something with a third workspace in the group: %v", fake.Calls())
	}
	// The generated anchor stays: untidy, and the price of not guessing.
	if n := len(fake.State().Workspaces); n != before+1 {
		t.Errorf("%d workspaces, want %d — the generated anchor and no more", n, before+1)
	}
	if _, ok := fake.Workspace("mine"); !ok {
		t.Error("the user's workspace was closed")
	}
}

// TestCmuxGroupKeepsTheAnchorWhenSetAnchorFails: the workspace is in
// the group either way, which is what the caller asked for, so a
// refused set-anchor leaves the generated header standing and reports
// nothing. Nothing is closed on that path — the group would lose its
// anchor.
func TestCmuxGroupKeepsTheAnchorWhenSetAnchorFails(t *testing.T) {
	fake := muxtest.InstallFakeCmux(t)
	fake.AddWorkspace("pr-1", "W1")
	fake.Reject("set-anchor")

	d := mux.NewCmux()
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W1", Name: "pr-1"}, mux.GroupStyle{}); err != nil {
		t.Fatalf("a refused set-anchor failed the call: %v", err)
	}
	g, ok := fake.Group("reviews")
	if !ok || !slices.Contains(g.MemberIDs, "W1") {
		t.Fatalf("group = %+v, %v; want W1 in it", g, ok)
	}
	if g.AnchorID == "W1" {
		t.Error("the anchor moved after set-anchor was refused")
	}
	if n := countCalls(fake.Calls(), "workspace close"); n != 0 {
		t.Errorf("closed the anchor the group still needs: %v", fake.Calls())
	}
}

// TestCmuxGroupAdoptsNothingWhenCmuxAnchorsOnGiven: a cmux that
// anchors the group on the workspace it was given leaves nothing to
// adopt — no set-anchor, no close. When cmux behaves this way, the
// adopt path can go.
func TestCmuxGroupAdoptsNothingWhenCmuxAnchorsOnGiven(t *testing.T) {
	fake := muxtest.InstallFakeCmux(t)
	fake.AddWorkspace("pr-1", "W1")
	fake.AnchorsOnGiven()

	d := mux.NewCmux()
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W1", Name: "pr-1"}, mux.GroupStyle{}); err != nil {
		t.Fatal(err)
	}
	g, _ := fake.Group("reviews")
	if g.AnchorID != "W1" || !reflect.DeepEqual(g.MemberIDs, []string{"W1"}) {
		t.Fatalf("group = %+v", g)
	}
	if countCalls(fake.Calls(), "workspace-group set-anchor") != 0 || countCalls(fake.Calls(), "workspace close") != 0 {
		t.Errorf("adopted an anchor that was already ours: %v", fake.Calls())
	}
}

// TestCmuxGroupAddClosesNothing: joining a group that exists is one
// add and nothing else — the adopt path belongs to creation.
func TestCmuxGroupAddClosesNothing(t *testing.T) {
	fake := muxtest.InstallFakeCmux(t)
	fake.AddWorkspace("pr-1", "W1")
	fake.AddWorkspace("pr-2", "W2")
	fake.AddGroup("reviews", "W1")

	d := mux.NewCmux()
	if err := mux.Group(d, "reviews", mux.Workspace{ID: "W2", Name: "pr-2"}, mux.GroupStyle{}); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"workspace close", "workspace-group set-anchor", "workspace-group create"} {
		if n := countCalls(fake.Calls(), verb); n != 0 {
			t.Errorf("%q ran %d times joining an existing group: %v", verb, n, fake.Calls())
		}
	}
}

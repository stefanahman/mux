package muxtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// FakeCmuxEnv names the directory the fake keeps its state in; set by
// InstallFakeCmux, read by FakeCmuxMain.
const FakeCmuxEnv = "MUXTEST_FAKE_CMUX"

// FakeCmuxState is what the fake cmux knows, kept in a file between
// invocations: the shapes the driver reads, as cmux 0.64.22 prints
// them, and what it was asked.
type FakeCmuxState struct {
	Next            int
	Workspaces      []FakeCmuxWorkspace
	Sessions        []FakeCmuxSession
	Notifications   []FakeCmuxNote
	Statuses        map[string][]FakeCmuxStatus // workspace id → sidebar status pills
	Groups          []FakeCmuxGroup             // sidebar workspace groups
	PS              FakePS                      // tty → foreground commands
	Typed           map[string][]string         // surface id → text and keys, in order
	Calls           []string
	Rejects         []string // verbs the fake fails, for the caller's error paths
	AnchorsOnGiven  bool     // create anchors on the workspace it was given, generating none
	CaptureOnCreate []string // workspace ids create also pulls into the group
	Selected        string   // workspace id
	Focused         string   // window ref
	EventsBootID    string   // boot_id the ack and the fake's own frames carry
	EventsGap       bool     // the ack reports a resume gap: replay was lost
}

// FakeCmuxWorkspace mirrors a `workspace list` record.
type FakeCmuxWorkspace struct {
	ID          string `json:"id"`
	Ref         string `json:"ref"`
	Title       string `json:"title"`
	CustomTitle string `json:"custom_title"`
	HasCustom   bool   `json:"has_custom_title"`
	Cwd         string `json:"current_directory"`
	Selected    bool   `json:"selected"`

	Panes []FakeCmuxPane `json:"panes"`
}

// FakeCmuxPane is a pane with its surfaces (tabs).
type FakeCmuxPane struct {
	Ref      string
	Surfaces []FakeCmuxSurface
}

// FakeCmuxSurface is one terminal surface. TTY is set for the surface
// a workspace was created with, as in cmux, and empty for a tab or
// split made through the API.
type FakeCmuxSurface struct {
	ID, Ref, Title, TTY string
}

// FakeCmuxSession mirrors a `sessions --agent claude` record.
type FakeCmuxSession struct {
	Workspace string `json:"workspace_id"`
	Lifecycle string `json:"agent_lifecycle"`
	PIDExists bool   `json:"stored_pid_exists"`
	UpdatedAt string `json:"updated_at"`
}

// FakeCmuxNote mirrors a `list-notifications` record.
type FakeCmuxNote struct {
	Workspace string `json:"workspace_id"`
	Read      bool   `json:"is_read"`
}

// FakeCmuxStatus is a sidebar status pill, what `set-status` writes
// and `list-status` prints.
type FakeCmuxStatus struct {
	Key, Value, Icon, Color, Priority string
}

// FakeCmuxGroup mirrors a `workspace-group list` record: the sidebar
// container, owned by the anchor workspace whose row is its header.
type FakeCmuxGroup struct {
	ID        string   `json:"id"`
	Ref       string   `json:"ref"`
	Name      string   `json:"name"`
	AnchorID  string   `json:"anchor_workspace_id"`
	AnchorRef string   `json:"anchor_workspace_ref"`
	MemberIDs []string `json:"member_workspace_ids"`
	Color     string   `json:"custom_color"`
	Icon      string   `json:"icon_symbol"`
}

// FakePS is a fake process table: per tty, the foreground command
// names. InstallFakeCmux puts a `ps` on PATH that prints it in the
// form the driver reads (`tty stat comm`).
type FakePS map[string][]string

func (w FakeCmuxWorkspace) name() string {
	if w.HasCustom {
		return w.CustomTitle
	}
	return w.Title
}

func fakeCmuxStatePath() string { return filepath.Join(os.Getenv(FakeCmuxEnv), "state.json") }

func loadFakeCmux() FakeCmuxState {
	var st FakeCmuxState
	if data, err := os.ReadFile(fakeCmuxStatePath()); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.PS == nil {
		st.PS = FakePS{}
	}
	if st.Typed == nil {
		st.Typed = map[string][]string{}
	}
	if st.Statuses == nil {
		st.Statuses = map[string][]FakeCmuxStatus{}
	}
	return st
}

// saveFakeCmux writes the state whole: the driver runs several fakes
// side by side, and one must never read another's half-written file.
func saveFakeCmux(st FakeCmuxState) {
	data, _ := json.Marshal(st)
	tmp := fakeCmuxStatePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err == nil {
		_ = os.Rename(tmp, fakeCmuxStatePath())
	}
}

// lockFakeCmux serialises the fakes' read-modify-write of the state
// file, so parallel invocations lose neither calls nor changes.
func lockFakeCmux() (unlock func()) {
	f, err := os.OpenFile(filepath.Join(os.Getenv(FakeCmuxEnv), "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// FakeCmuxMain is the CLI: the verbs the driver uses, with cmux's
// option spelling, answering from the state file. A test binary
// serves it when run under the name cmux:
//
//	func TestMain(m *testing.M) {
//		if filepath.Base(os.Args[0]) == "cmux" {
//			os.Exit(muxtest.FakeCmuxMain(os.Args[1:]))
//		}
//		os.Exit(m.Run())
//	}
func FakeCmuxMain(args []string) int {
	// `events` is the one verb that does not return: it streams until
	// the test ends it. It records its call, drops the lock, and then
	// serves — holding the lock for the life of a stream would block
	// every other call the driver makes.
	if len(args) > 0 && args[0] == "events" {
		unlock := lockFakeCmux()
		st := loadFakeCmux()
		st.Calls = append(st.Calls, strings.Join(args, " "))
		saveFakeCmux(st)
		boot, gap := st.EventsBootID, st.EventsGap
		unlock()
		return fakeCmuxEvents(boot, gap)
	}
	defer lockFakeCmux()()
	st := loadFakeCmux()
	st.Calls = append(st.Calls, strings.Join(args, " "))
	defer func() { saveFakeCmux(st) }()

	// Global presentation flags first, then the verb, then --opt value
	// pairs, flags without a value, and positionals.
	opts := map[string]string{}
	var words []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--json":
		case strings.HasPrefix(a, "--") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--"):
			opts[a] = args[i+1]
			i++
		case strings.HasPrefix(a, "--"):
			opts[a] = "true"
		default:
			words = append(words, a)
		}
	}
	fail := func(msg string) int { fmt.Fprintln(os.Stderr, "cmux: "+msg); return 1 }
	rejected := func(verb string) bool {
		for _, r := range st.Rejects {
			if r == verb {
				return true
			}
		}
		return false
	}
	group := func(handle string) (int, bool) {
		for i, g := range st.Groups {
			if g.ID == handle || g.Ref == handle {
				return i, true
			}
		}
		return 0, false
	}
	find := func(handle string) (int, bool) {
		for i, w := range st.Workspaces {
			if w.Ref == handle || w.ID == handle {
				return i, true
			}
		}
		return 0, false
	}
	surface := func(handle string) (wi, pi, si int, ok bool) {
		for wi, w := range st.Workspaces {
			for pi, p := range w.Panes {
				for si, s := range p.Surfaces {
					if s.ID == handle || s.Ref == handle {
						return wi, pi, si, true
					}
				}
			}
		}
		return 0, 0, 0, false
	}
	newSurface := func(title string) FakeCmuxSurface {
		st.Next++
		return FakeCmuxSurface{ID: fmt.Sprintf("SF-%d", st.Next), Ref: fmt.Sprintf("surface:%d", st.Next), Title: title}
	}
	out := func(v any) int {
		data, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(data))
		return 0
	}
	verb := strings.Join(words, " ")
	switch {
	case verb == "ping":
		fmt.Println("PONG")
	case verb == "identify":
		return out(map[string]any{"app_executable_path": "/nonexistent/fake-cmux.app/Contents/MacOS/cmux", "socket_path": "/tmp/fake-cmux.sock"})
	case verb == "tree" && opts["--all"] == "true":
		type surface struct {
			ID    string `json:"id"`
			Ref   string `json:"ref"`
			Title string `json:"title"`
			TTY   string `json:"tty"`
		}
		type pane struct {
			Ref         string    `json:"ref"`
			SurfaceIDs  []string  `json:"surface_ids"`
			SurfaceRefs []string  `json:"surface_refs"`
			Surfaces    []surface `json:"surfaces"`
		}
		type workspace struct {
			ID    string `json:"id"`
			Ref   string `json:"ref"`
			Title string `json:"title"`
			Panes []pane `json:"panes"`
		}
		var wss []workspace
		for _, w := range st.Workspaces {
			x := workspace{ID: w.ID, Ref: w.Ref, Title: w.name()}
			for _, p := range w.Panes {
				y := pane{Ref: p.Ref}
				for _, s := range p.Surfaces {
					y.SurfaceIDs, y.SurfaceRefs = append(y.SurfaceIDs, s.ID), append(y.SurfaceRefs, s.Ref)
					y.Surfaces = append(y.Surfaces, surface(s))
				}
				x.Panes = append(x.Panes, y)
			}
			wss = append(wss, x)
		}
		return out(map[string]any{"windows": []map[string]any{{"ref": "window:1", "workspaces": wss}}})
	case verb == "workspace list":
		return out(map[string]any{"window_ref": "window:1", "workspaces": st.Workspaces})
	case verb == "workspace create":
		st.Next++
		w := FakeCmuxWorkspace{ID: fmt.Sprintf("WS-%d", st.Next), Ref: fmt.Sprintf("workspace:%d", st.Next), Title: opts["--name"], CustomTitle: opts["--name"], HasCustom: opts["--name"] != "", Cwd: opts["--cwd"]}
		root := newSurface(w.name())
		root.TTY = fmt.Sprintf("ttys%03d", st.Next)
		w.Panes = []FakeCmuxPane{{Ref: fmt.Sprintf("pane:%d", st.Next), Surfaces: []FakeCmuxSurface{root}}}
		st.Workspaces = append(st.Workspaces, w)
		fmt.Println("OK " + w.Ref)
	case verb == "workspace select", verb == "workspace close", verb == "mark-notification-read", verb == "list-panes", verb == "list-pane-surfaces", verb == "new-surface", verb == "list-status",
		len(words) == 2 && words[0] == "clear-status", len(words) == 3 && words[0] == "set-status":
		i, ok := find(opts["--workspace"])
		if !ok {
			return fail("no such workspace " + opts["--workspace"])
		}
		w := st.Workspaces[i]
		switch verb {
		case "workspace select":
			st.Selected = w.ID
			fmt.Println("OK " + w.Ref)
		case "workspace close":
			st.Workspaces = append(st.Workspaces[:i], st.Workspaces[i+1:]...)
			// A closing workspace leaves its group; the anchor's row is
			// the group's header, so cmux promotes the next member, and
			// the last member out takes the group with it.
			var groups []FakeCmuxGroup
			for _, g := range st.Groups {
				var kept []string
				for _, m := range g.MemberIDs {
					if m != w.ID {
						kept = append(kept, m)
					}
				}
				if len(kept) == 0 {
					continue
				}
				g.MemberIDs = kept
				if g.AnchorID == w.ID {
					g.AnchorID = kept[0]
					for _, x := range st.Workspaces {
						if x.ID == kept[0] {
							g.AnchorRef = x.Ref
						}
					}
				}
				groups = append(groups, g)
			}
			st.Groups = groups
			fmt.Println("OK " + w.Ref)
		case "mark-notification-read":
			for j := range st.Notifications {
				if st.Notifications[j].Workspace == w.ID {
					st.Notifications[j].Read = true
				}
			}
			fmt.Println("OK")
		case "list-panes":
			panes := []map[string]any{}
			for _, p := range w.Panes {
				var ids, refs []string
				for _, s := range p.Surfaces {
					ids, refs = append(ids, s.ID), append(refs, s.Ref)
				}
				panes = append(panes, map[string]any{"ref": p.Ref, "surface_ids": ids, "surface_refs": refs})
			}
			return out(map[string]any{"workspace_ref": w.Ref, "panes": panes})
		case "list-pane-surfaces":
			pi := 0
			for j, p := range w.Panes {
				if p.Ref == opts["--pane"] {
					pi = j
				}
			}
			surfaces := []map[string]any{}
			for k, s := range w.Panes[pi].Surfaces {
				surfaces = append(surfaces, map[string]any{"id": s.ID, "ref": s.Ref, "title": s.Title, "index": k, "type": "terminal"})
			}
			return out(map[string]any{"workspace_ref": w.Ref, "pane_ref": w.Panes[pi].Ref, "surfaces": surfaces})
		case "new-surface":
			pi := 0
			for j, p := range w.Panes {
				if p.Ref == opts["--pane"] {
					pi = j
				}
			}
			s := newSurface("Terminal")
			st.Workspaces[i].Panes[pi].Surfaces = append(st.Workspaces[i].Panes[pi].Surfaces, s)
			fmt.Printf("OK %s %s %s\n", s.Ref, w.Panes[pi].Ref, w.Ref)
		case "list-status":
			// One line per pill, as cmux prints them; the value is not
			// quoted, so one with spaces runs into ` icon=`. A pill set
			// with a priority carries it as a fourth field.
			if len(st.Statuses[w.ID]) == 0 {
				fmt.Println("No status entries")
			}
			for _, p := range st.Statuses[w.ID] {
				fmt.Printf("%s=%s icon=%s color=%s", p.Key, p.Value, p.Icon, p.Color)
				if p.Priority != "" {
					fmt.Printf(" priority=%s", p.Priority)
				}
				fmt.Println()
			}
		default: // set-status <key> <value>, clear-status <key>
			key := words[1]
			var kept []FakeCmuxStatus
			for _, p := range st.Statuses[w.ID] {
				if p.Key != key {
					kept = append(kept, p)
				}
			}
			if words[0] == "set-status" {
				kept = append(kept, FakeCmuxStatus{Key: key, Value: words[2], Icon: opts["--icon"], Color: opts["--color"], Priority: opts["--priority"]})
			}
			st.Statuses[w.ID] = kept
			fmt.Println("OK")
		}
	case len(words) == 2 && words[0] == "new-split":
		wi, _, _, ok := surface(opts["--surface"])
		if !ok {
			return fail("no such surface " + opts["--surface"])
		}
		s := newSurface("Terminal")
		st.Next++
		st.Workspaces[wi].Panes = append(st.Workspaces[wi].Panes, FakeCmuxPane{Ref: fmt.Sprintf("pane:%d", st.Next), Surfaces: []FakeCmuxSurface{s}})
		fmt.Printf("OK %s %s\n", s.Ref, st.Workspaces[wi].Ref)
	case len(words) == 2 && words[0] == "rename-tab":
		wi, pi, si, ok := surface(opts["--surface"])
		if !ok {
			return fail("no such surface " + opts["--surface"])
		}
		st.Workspaces[wi].Panes[pi].Surfaces[si].Title = words[1]
		fmt.Println("OK")
	case len(words) == 2 && (words[0] == "send" || words[0] == "send-key"):
		_, _, _, ok := surface(opts["--surface"])
		if !ok {
			return fail(words[0] + ": no such surface " + opts["--surface"])
		}
		wi, pi, si, _ := surface(opts["--surface"])
		id := st.Workspaces[wi].Panes[pi].Surfaces[si].ID
		if words[0] == "send-key" {
			st.Typed[id] = append(st.Typed[id], "<"+words[1]+">")
		} else {
			st.Typed[id] = append(st.Typed[id], words[1])
		}
		fmt.Println("OK " + opts["--surface"])
	case verb == "workspace-group list":
		return out(map[string]any{"window_ref": "window:1", "window_id": "WIN-1", "groups": st.Groups})
	case verb == "workspace-group create":
		// --from names the workspaces to capture, and cmux 0.64.22 makes
		// an anchor of its own regardless: a workspace titled after the
		// group, in $HOME, listed first among the members. Verified live
		// — `create --name X --from <ws>` answers with a group of two.
		// Whoever reads this after a cmux that anchors on the workspace
		// it was given can delete the generated one here and in the
		// driver together.
		if opts["--from"] == "" {
			return fail("workspace-group create: --from is required here")
		}
		from := strings.Split(opts["--from"], ",")
		for _, id := range from {
			if _, ok := find(id); !ok {
				return fail("no such workspace " + id)
			}
		}
		if rejected("create") {
			return fail("create: rejected")
		}
		st.Next++
		var members []string
		anchorID, anchorRef := "", ""
		if !st.AnchorsOnGiven {
			anchor := FakeCmuxWorkspace{
				ID:          fmt.Sprintf("WS-ANCHOR-%d", st.Next),
				Ref:         fmt.Sprintf("workspace:%d", st.Next),
				Title:       opts["--name"],
				CustomTitle: opts["--name"],
				HasCustom:   opts["--name"] != "",
				Cwd:         os.Getenv("HOME"),
			}
			st.Workspaces = append(st.Workspaces, anchor)
			members = append(members, anchor.ID)
			anchorID, anchorRef = anchor.ID, anchor.Ref
		}
		for _, id := range append(from, st.CaptureOnCreate...) {
			i, ok := find(id)
			if !ok {
				continue
			}
			w := st.Workspaces[i]
			if slices.Contains(members, w.ID) {
				continue
			}
			members = append(members, w.ID)
			if anchorID == "" {
				anchorID, anchorRef = w.ID, w.Ref
			}
		}
		g := FakeCmuxGroup{
			ID:        fmt.Sprintf("GRP-%d", st.Next),
			Ref:       fmt.Sprintf("workspace_group:%d", st.Next),
			Name:      opts["--name"],
			AnchorID:  anchorID,
			AnchorRef: anchorRef,
			MemberIDs: members,
		}
		st.Groups = append(st.Groups, g)
		fmt.Println("OK " + g.Ref)
	case verb == "workspace-group set-anchor":
		if rejected("set-anchor") {
			return fail("set-anchor: rejected")
		}
		gi, ok := group(opts["--group"])
		if !ok {
			return fail("no such group " + opts["--group"])
		}
		wi, ok := find(opts["--workspace"])
		if !ok {
			return fail("no such workspace " + opts["--workspace"])
		}
		w := st.Workspaces[wi]
		if !slices.Contains(st.Groups[gi].MemberIDs, w.ID) {
			return fail("workspace is not in the group")
		}
		st.Groups[gi].AnchorID, st.Groups[gi].AnchorRef = w.ID, w.Ref
		fmt.Println("OK")
	case verb == "workspace-group add":
		if rejected("add") {
			return fail("add: rejected")
		}
		gi, ok := group(opts["--group"])
		if !ok {
			return fail("no such group " + opts["--group"])
		}
		wi, ok := find(opts["--workspace"])
		if !ok {
			return fail("no such workspace " + opts["--workspace"])
		}
		id := st.Workspaces[wi].ID
		for _, m := range st.Groups[gi].MemberIDs {
			if m == id {
				return fail("workspace already in the group")
			}
		}
		st.Groups[gi].MemberIDs = append(st.Groups[gi].MemberIDs, id)
		fmt.Println("OK " + st.Groups[gi].Ref)
	case verb == "workspace-group remove":
		wi, ok := find(opts["--workspace"])
		if !ok {
			return fail("no such workspace " + opts["--workspace"])
		}
		id := st.Workspaces[wi].ID
		for gi := range st.Groups {
			var kept []string
			for _, m := range st.Groups[gi].MemberIDs {
				if m != id {
					kept = append(kept, m)
				}
			}
			st.Groups[gi].MemberIDs = kept
		}
		fmt.Println("OK")
	case len(words) >= 2 && words[0] == "workspace-group" && (words[1] == "set-color" || words[1] == "set-icon"):
		if rejected(words[1]) {
			return fail(words[1] + ": rejected")
		}
		if len(words) < 3 {
			return fail(words[1] + ": a group is required")
		}
		gi, ok := group(words[2])
		if !ok {
			return fail("no such group " + words[2])
		}
		if words[1] == "set-color" {
			st.Groups[gi].Color = opts["--hex"]
		} else {
			st.Groups[gi].Icon = opts["--symbol"]
		}
		fmt.Println("OK " + st.Groups[gi].Ref)
	case verb == "sessions":
		return out(map[string]any{"sessions": st.Sessions, "total_matches": len(st.Sessions)})
	case verb == "list-notifications":
		return out(st.Notifications)
	case verb == "focus-window":
		st.Focused = opts["--window"]
		fmt.Println("OK " + st.Focused)
	case verb == "notify":
		fmt.Println("OK note")
	default:
		return fail("unknown command " + verb)
	}
	return 0
}

// fakeCmuxEventsPath is the queue the test pushes frames onto; the
// cursor beside it is how many lines a stream has already served, so a
// reconnect continues where the last one stopped rather than replaying.
func fakeCmuxEventsPath() string { return filepath.Join(os.Getenv(FakeCmuxEnv), "events.ndjson") }
func fakeCmuxCursorPath() string { return filepath.Join(os.Getenv(FakeCmuxEnv), "events.cursor") }

// fakeCmuxEndFrame is the sentinel that ends a stream: the test's way
// of saying the subscription dropped or cmux went away.
const fakeCmuxEndFrame = `{"__muxtest":"end"}`

// fakeCmuxEvents serves `cmux events`: the ack cmux sends on
// subscribing, then every frame the test pushes, until the sentinel or
// the process is killed.
func fakeCmuxEvents(boot string, gap bool) int {
	if boot == "" {
		boot = "BOOT-1"
	}
	fmt.Printf(`{"type":"ack","protocol":"cmux-events","version":1,"boot_id":%q,"subscription_id":"SUB-1","resume":{"gap":%t,"latest_seq":1,"next_seq":2}}`+"\n", boot, gap)
	os.Stdout.Sync()
	for {
		served := 0
		if b, err := os.ReadFile(fakeCmuxCursorPath()); err == nil {
			fmt.Sscanf(string(b), "%d", &served)
		}
		lines := []string{}
		if b, err := os.ReadFile(fakeCmuxEventsPath()); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if strings.TrimSpace(l) != "" {
					lines = append(lines, l)
				}
			}
		}
		if served < len(lines) {
			line := lines[served]
			_ = os.WriteFile(fakeCmuxCursorPath(), []byte(fmt.Sprint(served+1)), 0o644)
			if strings.Contains(line, `"__muxtest":"end"`) {
				return 0
			}
			fmt.Println(line)
			os.Stdout.Sync()
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// FakeCmux seeds and reads the fake's state from the test side.
type FakeCmux struct {
	t        *testing.T
	dir      string
	eventSeq int
	boot     string
}

// InstallFakeCmux puts the fake first on PATH — a symlink named cmux to
// the test binary, whose TestMain serves FakeCmuxMain — and clears the
// variables a cmux terminal would carry, so the test decides what is
// "inside".
func InstallFakeCmux(t *testing.T) *FakeCmux {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "cmux")); err != nil {
		t.Fatal(err)
	}
	// ps prints the fake process table, in the driver's format.
	ps := "#!/bin/sh\nwhile IFS= read -r line; do printf '%s\\n' \"$line\"; done < \"$" + FakeCmuxEnv + "/ps.txt\" 2>/dev/null; exit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(ps), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(FakeCmuxEnv, dir)
	t.Setenv("CMUX_WORKSPACE_ID", "")
	t.Setenv("CMUX_SURFACE_ID", "")
	t.Setenv("HERDR_ENV", "")
	saveFakeCmux(FakeCmuxState{})
	return &FakeCmux{t: t, dir: dir}
}

// State is a snapshot of the fake.
func (f *FakeCmux) State() FakeCmuxState { return loadFakeCmux() }

// Edit changes the state under the fake.
func (f *FakeCmux) Edit(fn func(st *FakeCmuxState)) {
	st := loadFakeCmux()
	fn(&st)
	saveFakeCmux(st)
}

// Workspace is the workspace with that name.
func (f *FakeCmux) Workspace(name string) (FakeCmuxWorkspace, bool) {
	for _, w := range f.State().Workspaces {
		if w.name() == name {
			return w, true
		}
	}
	return FakeCmuxWorkspace{}, false
}

// Typed is what was sent to a surface, text and keys.
func (f *FakeCmux) Typed(surfaceID string) []string { return f.State().Typed[surfaceID] }

// Calls is every invocation of the fake, arguments joined by spaces.
func (f *FakeCmux) Calls() []string { return f.State().Calls }

// AddWorkspace seeds a workspace with one surface; an empty name is a
// workspace cmux titled after its directory, like the home one.
func (f *FakeCmux) AddWorkspace(name, id string) {
	f.Edit(func(st *FakeCmuxState) {
		st.Next++
		w := FakeCmuxWorkspace{ID: id, Ref: fmt.Sprintf("workspace:%d", st.Next), Title: name, CustomTitle: name, HasCustom: name != ""}
		if name == "" {
			w.Title = "~"
		}
		w.Panes = []FakeCmuxPane{{Ref: fmt.Sprintf("pane:%d", st.Next), Surfaces: []FakeCmuxSurface{{ID: "SF-" + id, Ref: fmt.Sprintf("surface:%d", st.Next), Title: w.Title, TTY: fmt.Sprintf("ttys%03d", st.Next)}}}}
		st.Workspaces = append(st.Workspaces, w)
	})
}

// AddSession seeds a hook-store record.
func (f *FakeCmux) AddSession(workspaceID, lifecycle string, live bool, updated string) {
	f.Edit(func(st *FakeCmuxState) {
		st.Sessions = append(st.Sessions, FakeCmuxSession{Workspace: workspaceID, Lifecycle: lifecycle, PIDExists: live, UpdatedAt: updated})
	})
}

// AddStatus seeds a sidebar status pill: cmux's own under `claude_code`
// (icon and colour, no priority), claude-status's under `claude`, set
// with the priority its hooks pass.
func (f *FakeCmux) AddStatus(workspaceID, key, value string) {
	f.Edit(func(st *FakeCmuxState) {
		pill := FakeCmuxStatus{Key: key, Value: value, Icon: "bell.fill", Color: "#4C8DFF"}
		if key == "claude" {
			pill.Priority = "90"
		}
		st.Statuses[workspaceID] = append(st.Statuses[workspaceID], pill)
	})
}

// AddGroup seeds a group anchored on the first workspace given.
func (f *FakeCmux) AddGroup(name string, workspaceIDs ...string) {
	f.Edit(func(st *FakeCmuxState) {
		st.Next++
		g := FakeCmuxGroup{ID: fmt.Sprintf("GRP-%d", st.Next), Ref: fmt.Sprintf("workspace_group:%d", st.Next), Name: name, MemberIDs: workspaceIDs}
		if len(workspaceIDs) > 0 {
			g.AnchorID = workspaceIDs[0]
		}
		st.Groups = append(st.Groups, g)
	})
}

// Groups is what the fake holds, in cmux's order.
func (f *FakeCmux) Groups() []FakeCmuxGroup { return f.State().Groups }

// Group is the group with that name.
func (f *FakeCmux) Group(name string) (FakeCmuxGroup, bool) {
	for _, g := range f.Groups() {
		if g.Name == name {
			return g, true
		}
	}
	return FakeCmuxGroup{}, false
}

// AnchorsOnGiven makes create anchor the group on the workspace it was
// given and generate none — a cmux later than 0.64.22, or one asked
// through a path that does not synthesise a header row.
func (f *FakeCmux) AnchorsOnGiven() {
	f.Edit(func(st *FakeCmuxState) { st.AnchorsOnGiven = true })
}

// CaptureOnCreate makes create pull these workspaces into the group
// besides the one it was given, the way capturing an ambient selection
// would. A group that comes back holding more than the caller's
// workspace and the generated anchor is one nothing may close.
func (f *FakeCmux) CaptureOnCreate(workspaceIDs ...string) {
	f.Edit(func(st *FakeCmuxState) { st.CaptureOnCreate = append(st.CaptureOnCreate, workspaceIDs...) })
}

// Reject makes the fake fail those verbs — the second word of a
// `workspace-group` subcommand — so a caller's error path can be
// tested against a cmux that refuses.
func (f *FakeCmux) Reject(verbs ...string) {
	f.Edit(func(st *FakeCmuxState) { st.Rejects = append(st.Rejects, verbs...) })
}

// AddNote seeds a notification.
func (f *FakeCmux) AddNote(workspaceID string, read bool) {
	f.Edit(func(st *FakeCmuxState) {
		st.Notifications = append(st.Notifications, FakeCmuxNote{Workspace: workspaceID, Read: read})
	})
}

// SetForeground sets the foreground commands on a tty, what the fake
// ps reports; the surface a workspace was created with has one.
func (f *FakeCmux) SetForeground(tty string, commands ...string) {
	f.Edit(func(st *FakeCmuxState) { st.PS[tty] = commands })
	f.writePS()
}

// SetTitle sets a surface's title, what cmux's shell integration
// reports: the running program, or the directory at a prompt.
func (f *FakeCmux) SetTitle(surfaceID, title string) {
	f.Edit(func(st *FakeCmuxState) {
		for wi := range st.Workspaces {
			for pi := range st.Workspaces[wi].Panes {
				for si := range st.Workspaces[wi].Panes[pi].Surfaces {
					if st.Workspaces[wi].Panes[pi].Surfaces[si].ID == surfaceID {
						st.Workspaces[wi].Panes[pi].Surfaces[si].Title = title
					}
				}
			}
		}
	})
}

// writePS renders the process table for the fake ps.
func (f *FakeCmux) writePS() {
	st := f.State()
	var b strings.Builder
	for tty, cmds := range st.PS {
		for _, c := range cmds {
			fmt.Fprintf(&b, "%s S+ %s\n", tty, c)
		}
	}
	_ = os.WriteFile(filepath.Join(f.dir, "ps.txt"), []byte(b.String()), 0o644)
}

// PushFrame queues a raw frame for the stream to print next.
func (f *FakeCmux) PushFrame(frame any) {
	data, err := json.Marshal(frame)
	if err != nil {
		f.t.Fatal(err)
	}
	fh, err := os.OpenFile(fakeCmuxEventsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.Write(append(data, '\n')); err != nil {
		f.t.Fatal(err)
	}
}

// PushEvent queues an event frame, the shape docs/events.md prints.
func (f *FakeCmux) PushEvent(name, category string, payload map[string]any) {
	f.eventSeq++
	f.PushFrame(map[string]any{
		"type": "event", "protocol": "cmux-events", "version": 1,
		"boot_id": f.bootID(), "seq": f.eventSeq,
		"id":   fmt.Sprintf("%s-%d", f.bootID(), f.eventSeq),
		"name": name, "category": category, "source": "muxtest",
		"occurred_at": "2026-09-11T09:00:00.000Z",
		"payload":     payload,
	})
}

// PushStatus queues the event cmux emits when a sidebar pill is set:
// the workspace is not in workspace_id but inside the recorded command
// line, and the value is unquoted, so cmux's own `claude_code Needs
// input …` carries a space.
func (f *FakeCmux) PushStatus(workspaceID, key, value string) {
	args := fmt.Sprintf("%s %s --tab=%s --icon bolt.fill --color '#dbbc7f' --priority 90", key, value, workspaceID)
	f.PushEvent("sidebar.metadata.updated", "sidebar", map[string]any{"command": "set_status", "args": args})
}

// PushStatusCleared queues the event for a pill removed.
func (f *FakeCmux) PushStatusCleared(workspaceID, key string) {
	args := fmt.Sprintf("%s --tab=%s", key, workspaceID)
	f.PushEvent("sidebar.metadata.cleared", "sidebar", map[string]any{"command": "clear_status", "args": args})
}

// PushHeartbeat queues the keep-alive cmux sends every 15 s.
func (f *FakeCmux) PushHeartbeat() {
	f.PushFrame(map[string]any{"type": "heartbeat", "protocol": "cmux-events", "version": 1, "boot_id": f.bootID(), "subscription_id": "SUB-1", "latest_seq": f.eventSeq})
}

// EndStream ends the current stream where it stands: what a dropped
// subscription or a cmux that went away looks like from outside.
func (f *FakeCmux) EndStream() {
	f.PushFrame(map[string]any{"__muxtest": "end"})
}

// SetEvents decides what the next stream's ack says: which boot it
// belongs to, and whether the replay it would have resumed from is
// gone. A different boot id is cmux restarted.
func (f *FakeCmux) SetEvents(bootID string, gap bool) {
	f.boot = bootID
	f.Edit(func(st *FakeCmuxState) { st.EventsBootID, st.EventsGap = bootID, gap })
}

// bootID is what the fake stamps on the frames it queues.
func (f *FakeCmux) bootID() string {
	if f.boot == "" {
		return "BOOT-1"
	}
	return f.boot
}

// Streams counts the `events` subscriptions opened so far, so a test
// can wait for a reconnect.
func (f *FakeCmux) Streams() int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "events") {
			n++
		}
	}
	return n
}

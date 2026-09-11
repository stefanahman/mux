// Watching cmux instead of asking it. cmux publishes an event stream
// over its socket (`cmux events`, docs/events.md): newline-delimited
// JSON, an ack on subscribing, heartbeats every 15 s, and one frame
// per change. Reading it costs one process for as long as the caller
// watches, where asking cost one process per workspace per refresh —
// 36 of them every two seconds on a machine with 36 workspaces, which
// is what this replaces.
package mux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// cmuxView is everything States() reads. A watch keeps one current so
// that States() answers from memory; without a watch it is read on the
// spot, as before.
type cmuxView struct {
	list     cmuxList
	pills    map[string]string      // workspace id → claude-status's pill
	sessions map[string]cmuxSession // workspace id → cmux's own live record
	unread   map[string]bool        // workspace id → a notification not looked at
}

// What a frame says has gone stale. The pills are kept from the frames
// themselves — set_status and clear_status are the only writers, so
// applying them is exact — and the other three are re-read, because
// their events say that something changed without saying what it is
// now.
type cmuxStale uint8

const (
	cmuxStaleList cmuxStale = 1 << iota
	cmuxStaleSessions
	cmuxStaleUnread
	cmuxStalePills
	cmuxStaleAll = cmuxStaleList | cmuxStaleSessions | cmuxStaleUnread | cmuxStalePills
)

// cmuxSettle is how long a burst is allowed to finish before the stale
// parts are re-read. cmux emits an agent hook event per tool call, so a
// working agent produces them in runs; one read after the run is the
// same answer for a fraction of the work.
const cmuxSettle = 300 * time.Millisecond

// cmuxRetry is the pause before a dropped stream is opened again.
const cmuxRetry = time.Second

// cmuxWatch is a live subscription and the view it keeps.
type cmuxWatch struct {
	mu     sync.Mutex
	socket string // the app this watch follows; "" is the CLI's own default
	view   cmuxView
	dirty  cmuxStale
	closed bool
	sig    chan struct{} // to the caller; buffered 1, so signals coalesce
	wake   chan struct{} // to the refresher; same
}

// Watch subscribes to cmux's events and keeps this driver's view of
// every workspace's state current from them.
//
// The first read is done here, synchronously, so a caller that gets a
// channel back has a driver that already knows the answer — and one
// that cannot reach cmux gets the error now rather than silence.
func (c Cmux) Watch(ctx context.Context) (<-chan struct{}, error) {
	if c.cache == nil {
		return nil, errors.New("cmux: Watch needs the driver NewCmux returns; the zero value keeps nothing for a watch to fill")
	}
	if c.cache.watch != nil {
		return nil, errors.New("cmux: already watching")
	}
	view, err := readCmuxView(c.Socket)
	if err != nil {
		return nil, err
	}
	w := &cmuxWatch{view: view, socket: c.Socket, sig: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
	c.cache.watch = w
	go w.read(ctx)
	go w.refresh(ctx)
	return w.sig, nil
}

// readCmuxView reads the four things States() needs. It runs on its own
// driver, never the caller's: the watch reads from another goroutine,
// and two goroutines must not share one snapshot.
func readCmuxView(socket string) (cmuxView, error) {
	d := NewCmuxAt(socket)
	list, err := d.list()
	if err != nil {
		return cmuxView{}, err
	}
	pills, err := d.pills()
	if err != nil {
		return cmuxView{}, err
	}
	return cmuxView{list: list, pills: pills, sessions: d.sessions(), unread: d.unread()}, nil
}

// snapshot is the view as it stands, for States() to answer from. The
// maps in it are never written again once published — a change makes a
// new one — so the caller may read this one on its own goroutine.
func (w *cmuxWatch) snapshot() cmuxView {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.view
}

// stale marks parts of the view as needing a read and wakes the
// refresher. Marks accumulate: a burst of events is one read.
func (w *cmuxWatch) stale(s cmuxStale) {
	w.mu.Lock()
	w.dirty |= s
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// signal tells the caller that a later States() would answer
// differently. It never blocks and never sends after the watch closed.
func (w *cmuxWatch) signal() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	select {
	case w.sig <- struct{}{}:
	default:
	}
}

// close ends the caller's channel, once.
func (w *cmuxWatch) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.sig)
}

// read keeps a subscription open for as long as the context lives.
//
// The stream is not asked to reconnect for us (`cmux events
// --reconnect` would): a drop is exactly when the view may have missed
// something, and owning the loop is what makes the re-read certain.
// Every way a stream can end — the subscription dropped for falling
// behind, cmux restarted, the CLI died — lands here, and each means the
// same thing: read everything again.
func (w *cmuxWatch) read(ctx context.Context) {
	defer w.close()
	for ctx.Err() == nil {
		w.stream(ctx)
		if ctx.Err() != nil {
			return
		}
		w.stale(cmuxStaleAll)
		select {
		case <-ctx.Done():
			return
		case <-time.After(cmuxRetry):
		}
	}
}

// cmuxFrame is a frame of the stream: an ack, a heartbeat, or an event.
// Only the fields this driver acts on are named.
type cmuxFrame struct {
	Type     string `json:"type"`
	BootID   string `json:"boot_id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Payload  struct {
		Command string `json:"command"`
		Args    string `json:"args"`
	} `json:"payload"`
	Resume struct {
		Gap bool `json:"gap"`
	} `json:"resume"`
}

// stream runs one subscription to its end.
func (w *cmuxWatch) stream(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "cmux", "events",
		// sidebar carries the pill claude-status writes, which is the
		// state itself. The other three do not carry their answer, only
		// the news that it changed: a workspace opened or closed, a
		// notification was posted or read (which is `done` against
		// `idle`), an agent hook fired (cmux's own record, the fallback
		// where the pill is missing).
		"--category", "sidebar",
		"--category", "workspace",
		"--category", "notification",
		"--category", "agent")
	cmd.Env = append(cmd.Environ(), "CMUX_QUIET=1")
	cmd.Env = append(cmd.Env, Cmux{Socket: w.socket}.socketEnv()...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	defer func() { _ = cmd.Wait() }()

	boot := ""
	sc := bufio.NewScanner(out)
	// Frames are capped at 16 KiB by cmux, well inside the scanner's
	// default, but a truncated line would desynchronise the stream
	// rather than skip one frame.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var f cmuxFrame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			continue // a frame we cannot read is one we did not need
		}
		// A boot id that changes mid-stream is cmux restarted under us:
		// the sequence starts over and the view is from another process.
		if f.BootID != "" {
			if boot != "" && f.BootID != boot {
				w.stale(cmuxStaleAll)
			}
			boot = f.BootID
		}
		switch f.Type {
		case "ack":
			// A gap means the replay we would have resumed from is gone,
			// so what happened in between is unknown: read everything.
			if f.Resume.Gap {
				w.stale(cmuxStaleAll)
			}
		case "heartbeat":
			// Proof the socket lives. Nothing changed.
		case "event":
			w.apply(f)
		}
	}
}

// apply takes one event into the view.
func (w *cmuxWatch) apply(f cmuxFrame) {
	switch f.Name {
	case "sidebar.metadata.updated", "sidebar.metadata.cleared":
		key, value, tab, ok := parseStatusArgs(f.Payload.Args)
		if !ok || key != claudePill || tab == "" {
			return // another tool's pill, cmux's own among them
		}
		// A new map rather than a write into the old one: States() hands
		// the view out by value, so the maps inside it are read by the
		// caller's goroutine while this one is here.
		w.mu.Lock()
		pills := make(map[string]string, len(w.view.pills)+1)
		for k, v := range w.view.pills {
			pills[k] = v
		}
		if f.Payload.Command == "clear_status" {
			delete(pills, tab)
		} else {
			pills[tab] = value
		}
		w.view.pills = pills
		w.mu.Unlock()
		w.signal()
		return
	}
	switch f.Category {
	case "workspace":
		w.stale(cmuxStaleList)
	case "notification":
		w.stale(cmuxStaleUnread)
	case "agent":
		w.stale(cmuxStaleSessions)
	}
}

// parseStatusArgs reads the command line cmux records for a pill:
//
//	claude working --tab=<id> --icon bolt.fill --color '#dbbc7f' --priority 90
//	claude --tab=<id>                                        (a clear)
//
// The key comes first and the value runs to the first flag — cmux's own
// pill is `claude_code Needs input …`, unquoted, so a value can hold
// spaces. Flags come in both spellings, `--tab=x` and `--tab x`.
func parseStatusArgs(args string) (key, value, tab string, ok bool) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", "", "", false
	}
	key = fields[0]
	i := 1
	var words []string
	for ; i < len(fields) && !strings.HasPrefix(fields[i], "--"); i++ {
		words = append(words, fields[i])
	}
	value = strings.Join(words, " ")
	for ; i < len(fields); i++ {
		switch {
		case strings.HasPrefix(fields[i], "--tab="):
			tab = strings.TrimPrefix(fields[i], "--tab=")
		case fields[i] == "--tab" && i+1 < len(fields):
			tab = fields[i+1]
			i++
		}
	}
	return key, value, tab, true
}

// refresh re-reads what the frames said had changed, once a burst has
// settled, and tells the caller when it lands.
func (w *cmuxWatch) refresh(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cmuxSettle):
		}
		w.reread()
	}
}

// reread reads the stale parts on its own driver and stores them. The
// view is never emptied first: a caller reading States() meanwhile gets
// the last answer rather than none, and the signal that follows says a
// newer one is ready.
func (w *cmuxWatch) reread() {
	w.mu.Lock()
	dirty := w.dirty
	w.dirty = 0
	w.mu.Unlock()
	if dirty == 0 {
		return
	}

	d := NewCmuxAt(w.socket)
	var list *cmuxList
	var pills map[string]string
	if dirty&(cmuxStaleList|cmuxStalePills) != 0 {
		if l, err := d.list(); err == nil {
			list = &l
		}
	}
	if dirty&cmuxStalePills != 0 {
		if p, err := d.pills(); err == nil {
			pills = p
		}
	}
	var sessions map[string]cmuxSession
	if dirty&cmuxStaleSessions != 0 {
		sessions = d.sessions()
	}
	var unread map[string]bool
	if dirty&cmuxStaleUnread != 0 {
		unread = d.unread()
	}

	w.mu.Lock()
	if list != nil {
		w.view.list = *list
	}
	if pills != nil {
		w.view.pills = pills
	}
	if sessions != nil {
		w.view.sessions = sessions
	}
	if unread != nil {
		w.view.unread = unread
	}
	w.mu.Unlock()
	w.signal()
}

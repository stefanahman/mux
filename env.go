// A multiplexer's terminals inherit the environment of its server or
// app. An app launched from a shell gets that shell's environment
// (macOS's open hands it over), and two things in it break agents
// quietly: TMUX makes cmux's shell integration hand CMUX_SURFACE_ID to
// tmux before every command, so its Claude Code hooks never engage;
// Claude Code's own session markers make every agent a child session
// that saves no transcript. Ping catches both and says what to do.
package mux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Claude Code's session markers: an environment carrying them was
// started from inside a Claude Code session.
var claudeSessionMarkers = []string{"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION"}

// processEnv returns a process's environment as NAME=value entries.
// A variable, for tests.
var processEnv = func(pid int) ([]string, error) {
	switch runtime.GOOS {
	case "darwin":
		// ps -E appends the environment to the command line, space
		// separated; values with spaces are ambiguous, but the markers
		// looked for have none.
		out, err := runOut(exec.Command("ps", "-Eww", "-o", "command=", "-p", strconv.Itoa(pid)))
		if err != nil {
			return nil, err
		}
		var env []string
		for _, f := range strings.Fields(out) {
			if strings.Contains(f, "=") {
				env = append(env, f)
			}
		}
		return env, nil
	case "linux":
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
		if err != nil {
			return nil, err
		}
		return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), nil
	}
	return nil, errors.New("no way to read a process's environment on " + runtime.GOOS)
}

// socketOwner returns the pid of the process holding the Unix socket
// at path: lsof on macOS, ss on Linux; 0 when none is found or there
// is no way to look. A variable, for tests.
var socketOwner = func(path string) int {
	want := map[string]bool{path: true}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		want[r] = true
	}
	switch runtime.GOOS {
	case "darwin":
		// -F pn: one field per line, a process's p<pid> line before the
		// n<name> lines of its files; -U: Unix sockets only. A listener
		// names its path, a client only the peer's address.
		out, _ := exec.Command("lsof", "-U", "-F", "pn").Output()
		pid := 0
		for _, line := range strings.Split(string(out), "\n") {
			switch {
			case strings.HasPrefix(line, "p"):
				pid, _ = strconv.Atoi(line[1:])
			case strings.HasPrefix(line, "n") && want[line[1:]] && pid != 0:
				return pid
			}
		}
	case "linux":
		// -xlpH: listening Unix sockets, no header, the path in the
		// fifth column and users:(("name",pid=N,fd=M)) at the end.
		out, _ := exec.Command("ss", "-xlpH").Output()
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) < 5 || !want[f[4]] {
				continue
			}
			if _, after, ok := strings.Cut(line, "pid="); ok {
				digits := strings.TrimRightFunc(after, func(r rune) bool { return r < '0' || r > '9' })
				n, _ := strconv.Atoi(strings.SplitN(digits, ",", 2)[0])
				return n
			}
		}
	}
	return 0
}

// ownerEnv is the environment of the process holding the socket, and
// whether there is one to audit: not when nothing holds it, when the
// caller itself does (a fake in a test; nothing it could restart), or
// when it can't be read.
func ownerEnv(socket string) ([]string, bool) {
	pid := socketOwner(socket)
	if pid == 0 || pid == os.Getpid() {
		return nil, false
	}
	env, err := processEnv(pid)
	if err != nil {
		return nil, false
	}
	return env, true
}

// carries reports which of the names the environment sets.
func carries(env []string, names ...string) []string {
	var found []string
	for _, n := range names {
		for _, e := range env {
			if strings.HasPrefix(e, n+"=") {
				found = append(found, n)
				break
			}
		}
	}
	return found
}

// ErrTainted is what Ping's environment failures wrap: the server or
// app answers, but was started with something in its environment
// that breaks the agents in it. errors.Is(err, ErrTainted) tells it
// from an unreachable multiplexer.
var ErrTainted = errors.New("the multiplexer's environment breaks agents")

type taintError struct{ msg string }

func (e taintError) Error() string      { return e.msg }
func (taintError) Is(target error) bool { return target == ErrTainted }

// claudeTaint is the error for a server or app whose environment
// carries Claude Code's session markers, nil when it doesn't.
func claudeTaint(what string, env []string) error {
	if m := carries(env, claudeSessionMarkers...); len(m) > 0 {
		return taintError{fmt.Sprintf("%s was started from inside a Claude Code session (%s): agents started in it run as child sessions and save no transcript; restart it from a hotkey or a plain shell", what, strings.Join(m, ", "))}
	}
	return nil
}

// CleanEnv drops from env what a multiplexer's server or app must not
// inherit from the shell that starts it: tmux's, cmux's and herdr's
// own variables, and Claude Code's session markers. For launchers.
func CleanEnv(env []string) []string {
	return without(env, func(name string) bool {
		return name == "TMUX" || name == "TMUX_PANE" || isClaudeMarker(name) ||
			strings.HasPrefix(name, "CMUX_") || strings.HasPrefix(name, "HERDR_")
	})
}

// withoutClaude drops Claude Code's session markers only: for a
// command that may start a server yet must still find the current one.
func withoutClaude(env []string) []string { return without(env, isClaudeMarker) }

func isClaudeMarker(name string) bool {
	return name == "CLAUDECODE" || strings.HasPrefix(name, "CLAUDE_")
}

// without is env less the variables drop names.
func without(env []string, drop func(name string) bool) []string {
	var out []string
	for _, e := range env {
		if name, _, _ := strings.Cut(e, "="); !drop(name) {
			out = append(out, e)
		}
	}
	return out
}

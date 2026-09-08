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

// findProcess returns the pid of the first process whose command line
// starts with prefix; 0 when there is none. A variable, for tests.
var findProcess = func(prefix string) int {
	out, err := runOut(exec.Command("ps", "-axo", "pid=,command="))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(out, "\n") {
		pid, cmd, _ := strings.Cut(strings.TrimSpace(line), " ")
		if strings.HasPrefix(cmd, prefix) {
			if n, err := strconv.Atoi(pid); err == nil {
				return n
			}
		}
	}
	return 0
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

// claudeTaint is the error for a server or app whose environment
// carries Claude Code's session markers, nil when it doesn't.
func claudeTaint(what string, env []string) error {
	if m := carries(env, claudeSessionMarkers...); len(m) > 0 {
		return fmt.Errorf("%s was started from inside a Claude Code session (%s): agents started in it run as child sessions and save no transcript; restart it from a hotkey or a plain shell", what, strings.Join(m, ", "))
	}
	return nil
}

// CleanEnv drops from env what a multiplexer's server or app must not
// inherit from the shell that starts it: tmux's, cmux's and herdr's
// own variables, and Claude Code's session markers. For launchers.
func CleanEnv(env []string) []string {
	var out []string
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		switch {
		case name == "TMUX", name == "TMUX_PANE", name == "CLAUDECODE",
			strings.HasPrefix(name, "CLAUDE_"), strings.HasPrefix(name, "CMUX_"), strings.HasPrefix(name, "HERDR_"):
			continue
		}
		out = append(out, e)
	}
	return out
}

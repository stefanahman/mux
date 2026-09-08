package mux

import (
	"strings"
	"testing"
)

func TestCleanEnv(t *testing.T) {
	got := CleanEnv([]string{"HOME=/h", "TMUX=/t,1,2", "TMUX_PANE=%1", "CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=1", "CLAUDE_EFFORT=high", "CMUX_WORKSPACE_ID=x", "HERDR_ENV=1", "SSH_AUTH_SOCK=/s", "PATH=/p"})
	if strings.Join(got, " ") != "HOME=/h SSH_AUTH_SOCK=/s PATH=/p" {
		t.Errorf("CleanEnv = %v", got)
	}
}

func TestCarriesAndTaint(t *testing.T) {
	env := []string{"HOME=/h", "CLAUDE_CODE_CHILD_SESSION=1", "TMUX=/tmp/x,1,2"}
	if got := carries(env, "TMUX", "CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION"); strings.Join(got, ",") != "TMUX,CLAUDE_CODE_CHILD_SESSION" {
		t.Errorf("carries = %v", got)
	}
	if err := claudeTaint("x", env); err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_CHILD_SESSION") {
		t.Errorf("claudeTaint = %v", err)
	}
	if err := claudeTaint("x", []string{"HOME=/h"}); err != nil {
		t.Errorf("clean env: %v", err)
	}
}

// withProcess makes the process lookups answer from a table.
func withProcess(t *testing.T, pids map[string]int, envs map[int][]string) {
	t.Helper()
	oldFind, oldEnv := findProcess, processEnv
	findProcess = func(prefix string) int {
		for p, pid := range pids {
			if strings.HasPrefix(p, prefix) {
				return pid
			}
		}
		return 0
	}
	processEnv = func(pid int) ([]string, error) { return envs[pid], nil }
	t.Cleanup(func() { findProcess, processEnv = oldFind, oldEnv })
}

func TestAuditApp(t *testing.T) {
	app := "/Applications/cmux.app/Contents/MacOS/cmux"
	withProcess(t, map[string]int{app: 7}, map[int][]string{7: {"HOME=/h", "TMUX=/tmp/tmux-501/default,1,5"}})
	if err := auditApp(app); err == nil || !strings.Contains(err.Error(), "TMUX") || !strings.Contains(err.Error(), "hooks never engage") {
		t.Errorf("app launched with TMUX: %v", err)
	}
	withProcess(t, map[string]int{app: 7}, map[int][]string{7: {"HOME=/h", "CLAUDECODE=1"}})
	if err := auditApp(app); err == nil || !strings.Contains(err.Error(), "child sessions") {
		t.Errorf("app launched from Claude Code: %v", err)
	}
	withProcess(t, map[string]int{app: 7}, map[int][]string{7: {"HOME=/h", "CMUX_SOCKET_MODE=allowAll"}})
	if err := auditApp(app); err != nil {
		t.Errorf("clean app: %v", err)
	}
	withProcess(t, map[string]int{}, nil)
	if err := auditApp(app); err != nil {
		t.Errorf("no app process: %v", err)
	}
}

func TestAuditHerdrServer(t *testing.T) {
	withProcess(t, map[string]int{"herdr --session work server": 9}, map[int][]string{9: {"CLAUDE_CODE_CHILD_SESSION=1"}})
	if err := auditHerdrServer("work"); err == nil || !strings.Contains(err.Error(), "work server") {
		t.Errorf("tainted server: %v", err)
	}
	withProcess(t, map[string]int{"herdr --session work server": 9}, map[int][]string{9: {"HOME=/h"}})
	if err := auditHerdrServer("work"); err != nil {
		t.Errorf("clean server: %v", err)
	}
	withProcess(t, map[string]int{"herdr server": 3}, map[int][]string{3: {"CLAUDECODE=1"}})
	if err := auditHerdrServer("default"); err == nil || !strings.Contains(err.Error(), "default server") {
		t.Errorf("default session server: %v", err)
	}
}

package mux

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCleanEnv(t *testing.T) {
	env := []string{"HOME=/h", "TMUX=/t,1,2", "TMUX_PANE=%1", "CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=1", "CLAUDE_EFFORT=high", "CMUX_WORKSPACE_ID=x", "HERDR_ENV=1", "SSH_AUTH_SOCK=/s", "PATH=/p"}
	if got := strings.Join(CleanEnv(env), " "); got != "HOME=/h SSH_AUTH_SOCK=/s PATH=/p" {
		t.Errorf("CleanEnv = %q", got)
	}
	if got := strings.Join(withoutClaude(env), " "); got != "HOME=/h TMUX=/t,1,2 TMUX_PANE=%1 CMUX_WORKSPACE_ID=x HERDR_ENV=1 SSH_AUTH_SOCK=/s PATH=/p" {
		t.Errorf("withoutClaude = %q", got)
	}
}

func TestCarriesAndTaint(t *testing.T) {
	env := []string{"HOME=/h", "CLAUDE_CODE_CHILD_SESSION=1", "TMUX=/tmp/x,1,2"}
	if got := carries(env, "TMUX", "CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION"); strings.Join(got, ",") != "TMUX,CLAUDE_CODE_CHILD_SESSION" {
		t.Errorf("carries = %v", got)
	}
	if err := claudeTaint("x", env); err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_CHILD_SESSION") || !errors.Is(err, ErrTainted) {
		t.Errorf("claudeTaint = %v", err)
	}
	if err := claudeTaint("x", []string{"HOME=/h"}); err != nil {
		t.Errorf("clean env: %v", err)
	}
}

// withOwner makes the socket lookups answer from a table: the pid
// holding each socket, and each pid's environment.
func withOwner(t *testing.T, owners map[string]int, envs map[int][]string) {
	t.Helper()
	oldOwner, oldEnv := socketOwner, processEnv
	socketOwner = func(path string) int { return owners[path] }
	processEnv = func(pid int) ([]string, error) { return envs[pid], nil }
	t.Cleanup(func() { socketOwner, processEnv = oldOwner, oldEnv })
}

func TestOwnerEnv(t *testing.T) {
	withOwner(t, map[string]int{"/s": 7, "/me": os.Getpid()}, map[int][]string{7: {"A=1"}, os.Getpid(): {"CLAUDECODE=1"}})
	if env, ok := ownerEnv("/s"); !ok || strings.Join(env, " ") != "A=1" {
		t.Errorf("held socket: %v %v", env, ok)
	}
	if _, ok := ownerEnv("/none"); ok {
		t.Error("a socket nobody holds is audited")
	}
	if _, ok := ownerEnv("/me"); ok {
		t.Error("the caller's own socket is audited")
	}
}

func TestAuditCmuxApp(t *testing.T) {
	sock := "/state/cmux.sock"
	withOwner(t, map[string]int{sock: 7}, map[int][]string{7: {"HOME=/h", "TMUX=/tmp/tmux-501/default,1,5"}})
	if err := auditCmuxApp(sock); err == nil || !strings.Contains(err.Error(), "TMUX") || !strings.Contains(err.Error(), "hooks never engage") || !errors.Is(err, ErrTainted) {
		t.Errorf("app launched with TMUX: %v", err)
	}
	withOwner(t, map[string]int{sock: 7}, map[int][]string{7: {"HOME=/h", "CLAUDECODE=1"}})
	if err := auditCmuxApp(sock); err == nil || !strings.Contains(err.Error(), "child sessions") {
		t.Errorf("app launched from Claude Code: %v", err)
	}
	withOwner(t, map[string]int{sock: 7}, map[int][]string{7: {"HOME=/h", "CMUX_SOCKET_MODE=allowAll"}})
	if err := auditCmuxApp(sock); err != nil {
		t.Errorf("clean app: %v", err)
	}
	withOwner(t, nil, nil)
	if err := auditCmuxApp(sock); err != nil {
		t.Errorf("nobody holds the socket: %v", err)
	}
}

func TestAuditHerdrServer(t *testing.T) {
	sock := "/cfg/herdr/sessions/work/herdr.sock"
	withOwner(t, map[string]int{sock: 9}, map[int][]string{9: {"CLAUDE_CODE_CHILD_SESSION=1"}})
	if err := auditHerdrServer(sock, "work"); err == nil || !strings.Contains(err.Error(), "the work server") {
		t.Errorf("tainted server: %v", err)
	}
	withOwner(t, map[string]int{sock: 9}, map[int][]string{9: {"HOME=/h"}})
	if err := auditHerdrServer(sock, "work"); err != nil {
		t.Errorf("clean server: %v", err)
	}
}

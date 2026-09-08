package muxtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// StartTmux runs a private tmux server for the test: its own socket
// directory (Unix socket paths are short-limited and t.TempDir is
// long), its own minimal config (/bin/sh in every window — the
// developer's shell would write history into $HOME on exit), and the
// inherited $TMUX cleared so a test run from inside tmux never reaches
// the developer's server. Skips when tmux is not installed.
func StartTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sockDir, err := os.MkdirTemp("", "muxtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	conf := filepath.Join(sockDir, "tmux.conf")
	if err := os.WriteFile(conf, []byte("set -g default-shell /bin/sh\nset -s exit-empty off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", sockDir)
	t.Setenv("TMUX", "")
	t.Setenv("HISTFILE", "")
	if out, err := exec.Command("tmux", "-L", "default", "-f", conf, "start-server").CombinedOutput(); err != nil {
		t.Fatalf("tmux start-server: %v\n%s", err, out)
	}
	socket := filepath.Join(sockDir, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
}

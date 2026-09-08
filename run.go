package mux

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// runOut runs a command and returns trimmed stdout. Errors carry the
// command's stderr, which is where the useful message is.
func runOut(cmd *exec.Cmd) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s: %s", strings.Join(cmd.Args[:min(len(cmd.Args), 2)], " "), msg)
	}
	return strings.TrimSpace(string(out)), nil
}

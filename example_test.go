package mux_test

import "github.com/stefanahman/mux"

// The common path: find the multiplexer, make a workspace for a task,
// start an agent in it, and read back what every agent is doing.
func Example() {
	d := mux.Detect(mux.Tmux{SessionName: "work"}, mux.NewHerdr(""), mux.Cmux{})
	_ = d.Prepare("/repo")
	ws, _ := d.Create("pr-42", "/repo/.worktrees/pr-42")
	pane, _ := d.AgentPane(ws)
	_ = d.Run(ws, pane, `claude "review PR 42"`)
	states, _ := d.States() // states["pr-42"]: working, blocked, done or idle
	_ = states
}

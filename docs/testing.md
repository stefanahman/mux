# Testing against it

`muxtest` has doubles for all three: `NewFakeHerdr` serves herdr's
wire shapes on a socket, `InstallFakeCmux` puts a fake `cmux` on PATH
(the test binary itself — its `TestMain` hands the name to
`FakeCmuxMain`), `StartTmux` runs a private tmux server. All three
clear the variables herdr and cmux put in a terminal's environment,
so a test run from inside one still lands on its double — `Detect`
would otherwise reach the real thing. mux's own tests are the example.

```sh
make test   # -race; tmux on PATH runs the tmux tests, otherwise they skip
make lint
```

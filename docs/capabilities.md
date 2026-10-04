# Capabilities

What only some multiplexers can do. Each is an interface beside `Driver`, with a
helper that does nothing where the driver lacks it, so a caller never branches
on `Kind`.

`Group` is a capability, not a Driver verb: `mux.Group(d, name, ws,
style)` puts a workspace in a named group where the multiplexer has
them, and does nothing where it does not, so a caller never branches on
`Kind`. Only `Cmux` implements `Grouper`. The group is created on the
first workspace that needs it and anchored there, found by name
afterwards, and it disappears with its last member. `GroupStyle`'s
colour and SF Symbol are applied on creation and are best effort: a
style the multiplexer refuses leaves the workspace grouped and returns
no error.

Anchoring costs a step. cmux 0.64.22 answers `workspace-group create
--from <ws>` with a group of two — a workspace it generates to carry
the header, plus the one it was given — so the driver moves the anchor
onto the caller's workspace and closes the generated one. Left alone,
every group would put a phantom row in the sidebar and outlive its
real members. That close only fires on the shape a fresh create leaves,
a group of exactly the caller's workspace and one other; anything else
keeps its generated anchor, because an untidy sidebar is recoverable
and a closed workspace is not.

`Watch` is another capability. `mux.Watch(d, ctx)` returns a channel
that signals whenever a later `States()` would answer differently, and
`nil` for a driver without a stream — a nil channel blocks forever in a
select, so a caller can keep a timer beside it and never branch on
`Kind`. Only `Cmux` implements `Watcher`.

It exists because asking cmux was expensive. cmux has no bulk read for
sidebar pills, so `States()` runs one `list-status` per workspace: 36
processes per refresh on a machine with 36 workspaces, and a caller
refreshing every two seconds pays that over and over. cmux publishes an
event stream instead (its `docs/events.md`), and while a watch is live
`States()` answers from what the stream has told the driver, running
nothing.

Three things a caller has to get right, each of which cost owl a day:

- **One driver, kept.** `Watch` hangs the subscription off the instance
  it is called on. A caller that builds a driver per read subscribes one
  of them and goes on fanning out from all the others — the calls do not
  drop and nothing says why. Keep the driver that was watched for as
  long as the watch, and read through it.
- **A kept driver without a live watch freezes.** Reads fill a snapshot
  that only a mutating call clears; a watch bypasses it, which is what
  makes keeping the driver correct. If `Watch` failed and the driver is
  kept anyway, `States()` answers with the states it saw first, forever:
  no error, no staleness, a list that simply stops moving. Drop the
  driver whenever the watch does not start.
- **The timer still matters where there is no stream.** Under tmux and
  herdr `Watch` returns `nil` and the timer is the only thing that moves
  a state, so an interval chosen for a watched cmux is a regression
  there. Pick it from whether a watch is actually live — owl uses 30s
  watched, 2s not.

The pills come from the frames: `set_status` and `clear_status` are the
only writers, so applying them is exact. The workspace list, the hook
store and the notifications do not travel in their events — those say
only that something changed — so each is re-read when its own category
appears, after a burst has settled. A resume gap, a changed `boot_id`, a
subscription dropped for falling behind and a dead CLI all mean the
same thing: read everything again. The reconnect is the driver's own
rather than `cmux events --reconnect`, because a drop is exactly when
the view may have missed something.

`Move` is the third. `mux.Move(d, ws, index)` puts a workspace at an
index of the order `Workspaces()` lists — counted after the move, so
the index is where it ends up — and does nothing for a driver that
cannot reorder. Only `Herdr` implements `Mover`: herdr's own
`insert_index` counts from the order before the move, and the driver
asks for one more when moving right. spaces uses it to put a workspace
it makes later back where its file declares it.

# Turn lifecycle and recovery

A turn is one provider process supervised to a terminal record. This doc is
why the lifecycle observes what it does, why a turn outlives the caller that
dispatched it, why recovery follows prompt state alone, why session locks are
never reclaimed, and how a finished turn's conversation continues.
`internal/runner` owns the lifecycle as a single-threaded state machine;
`internal/detach` runs it outside the caller's processes; `internal/collect`
owns the eligibility, liveness and recovery reads.

## Process exit and stream EOF

A provider runs in its own process group, and process exit and stream EOF are
separate observations: a timed-out provider once left grandchildren holding
the stdio pipes, the stream-close event never fired, and the runner hung
(`EVIDENCE.md`, 2026-07-06). Stopping escalates exit → grace → SIGKILL over
the whole group, and the runner passes raw pipe fds rather than exec's managed
pipes, so waiting on the process observes its exit alone.

## A turn outlives its caller

A Claude Code session that ends — its tmux server dies, a dropped network
restarts it, a usage limit stops it — stops its background tasks the way
`TaskStop` stops one: SIGTERM to the task's process group and to every process
below it. A runner inside the caller's task dies with every session end, and
every interrupted turn in both stores was that collateral, none a decision
(`EVIDENCE.md`, 2026-10-08). So the turn runs in a process the caller does not
own, and the caller only waits for it:

1. **The waiter** is the `envoy run` the caller started. It relays the
   dispatch's stdout and stderr, reads the dispatch's PID and exit code from a
   control pipe, and exits with that code, so a caller that keeps waiting reads
   what an attached dispatch printed.
2. **An intermediate** leads a new session, starts the dispatch in it and exits
   at once, handing it to init: outside the caller's process group, outside the
   caller's tree once its parent is gone, and never a session leader, so it
   never acquires a controlling terminal. Go cannot fork, which is why it
   exists.
3. **The detached process** runs the dispatch exactly as an attached one would —
   validation, name reservation, the runner or the fan-out — and the waiter
   reads no flag or record of it. A detached process that cannot start
   dispatches nothing: there is one way a turn runs.

The signals split by who sends them. **SIGINT** — Ctrl-C on a dispatch, which a
harness never sends a task — is forwarded, so an interrupt, and a second one,
behave as they always have. **SIGTERM and SIGHUP** — what a dying harness or
terminal sends, and the only way Claude Code stops a task — stop the waiting
alone: the waiter says so and dies by that signal, and the turn runs on to its
end or its cap. That takes the task layer's cancel away, and callers do cancel —
a brief that grew, a suspected hang — so a live turn shows its **stop**, `kill
-INT <runner pid>`, wherever it is shown: collect, once for a whole fan-out, and
`pending`. It is a printed command, not a verb; a cancel service stays a
non-goal.

**`envoy wait <job>`** gives a caller that lost its waiter the completion signal
back: it blocks on the runner's claim, then prints the block the dispatch ends
on and exits with the dispatch's code. A wait delivers nothing — no result, no
stamp, no reconciliation — because a background command's output reaches no
reader, and its block sends the caller to collect. A turn whose runner left no
final record, or a fan-out with a member that did, is reported as an ending the
records do not hold, for collect to read; collect in turn offers a wait only
while the turn's process lives, so collect and wait never send a caller round
a loop.

The close-hang lesson applies one level up. The detached process holds the
relays as its stdout and stderr, so a panic still reaches a waiting caller, and
catches SIGPIPE — never ignores it, since an ignored signal is inherited by the
provider — so a write after the waiter is gone fails instead of killing it. Its
control descriptor is close-on-exec, so no provider or grandchild holds the
waiter open. On Linux a stopped systemd scope or cgroup still reaches the turn:
the guarantee is the process tree, no more.

Rejected:

- **A `--detach` that returns at once:** callers lose the exit as completion
  signal and poll (`docs/README.md` § Commitments that hold everywhere).
- **Leaving it to callers:** sessions hand-rolled it the day it bit, each its
  own way and each beside a polling waiter, one matching statuses envoy never
  writes (`EVIDENCE.md`, 2026-10-08); macOS ships no `setsid` command.
- **Ignoring SIGTERM in the runner, or a `setsid` child of the waiter:** the
  teardown reaches every descendant either way.
- **`collect --wait`:** a background collect would stamp a block nobody read.

## A runner is alive while it holds its claim

The runner, and a fan-out's process, take an exclusive `flock` on the job
directory before the first record and hold it until they return — past the
final record and the session lock's release — and the record says so
(`runnerLock`). Every reader asking whether a runner is alive — collect, wait,
name holds, `pending`, a session lock's refusal — reads that claim
(`proc.RunnerLiveness`), and the PID only for a record without one. A PID that
answers after a crash or a reboot can belong to any process; read as a live
runner, it would make a wait block forever and point a stop at a stranger. The
claim's descriptor is close-on-exec, so a provider never inherits it.

## The timeout is a safety cap, not a stall detector

The cap is a wall-clock deadline compared against a fixed instant, so laptop
sleep cannot stretch it. Healthy deep work and a hang are indistinguishable
from outside, so the engine never *acts* on quiet: there is no activity-based
hang detector and no timeout that resets on output.

A capped turn's envelope does *report* the stream it observed — events seen,
the last event, how long it had been quiet — because a cap alone reads
identically whether the provider worked to the deadline or went silent right
after acceptance, and those want opposite follow-ups. With only the cap to go
on, one caller diagnosed a stall by hand and the next wrote "finish what you
started" into a session that had produced nothing (`EVIDENCE.md`,
2026-07-31). Not acting on an observation is no reason to withhold it; the
stream is read before teardown, since one read after it describes envoy's own
cleanup.

**The terminal envelope wins.** If the cap fires while an already-complete
turn is draining, the observed success is published, not a timeout.

## Recovery follows prompt state

Prompt state is the only thing the engine can prove about a turn that did not
return a result, and each value licenses one action:

- **`accepted`** → resume the session with a new prompt, never redispatch:
  work may already exist in the session and the tree.
- **`not_started`** → one identical retry is safe.
- **`unknown`** → absence of output is not proof of no work; read the logs and
  the tree first.

Re-sending a prompt the provider already accepted duplicates work that may
already have changed the tree — the expensive mistake this rule exists to
prevent. What the session and the process show outranks prompt state, because
acting on it beside them would put two turns on one conversation or one tree
(`recoveryAction`, `recoveryForStale` in `internal/collect`):

- **A held session.** A turn refused for a lock conflict is sent to the job
  that owns the session, whatever its prompt state.
- **A runner gone while its turn is recorded running.** A provider still alive
  as an orphan is waited for or stopped first, and process state that cannot
  be read is inspected, not acted on. Only a provably abandoned turn is
  reconciled and recovered by its prompt state.

Every other failure path routes to one of the prompt states (`prose.Recovery`).

## Session locks

One live turn per session id, including an id that only arrives mid-stream.
A lock is refused, never reclaimed: a dead runner can leave a live orphan
provider, and automatic stale takeover cannot be made race-free with a plain
lock file, so the engine only ever says no and points at the job owning the
session, to inspect first. Correctness of a shared conversation outranks
convenience. Locks live in `~/.local/state/envoy/locks/`, and a refusal
records the lock file's account of its holder
(`docs/records-and-collection.md`), live while its job's claim is held.

## Continuation

`--with @<job>` continues a finished job's conversation as a new job, anchored
on the job's records rather than a remembered session id: a resumed claude
conversation continues under a fresh id, so a remembered id silently forks the
conversation at a stale point, while the records always name the current head.
The continued voice reads its provider, session, model, effort, tree, baseline
and write intent from the job's `meta.json`; explicit `--cwd` and
`--baseline` win, and every continued turn records `resumedFrom`.

What carries over, and why (`EVIDENCE.md`, 2026-07-29):

- **The cap never inherits.** It is phase policy — a 30-minute consult
  continues into a 60-minute review — so it is always the new dispatch's own.
- **Write intent inherits and may only widen.** A write-recorded source
  continues alone, since fan-out members are read-only.
- **The provider and model come from the record.** Carrying context to another
  model family is prompt authorship, not continuation.

Eligibility has one definition, `continuationBlocker` in `internal/collect`,
consulted by collect's `resume:` line, the fan-out's set-level line and
dispatch's `@<job>` inspection, so collect never advertises a continuation
dispatch would refuse. A running turn, a recorded lock conflict and a turn
with no session cannot continue, and a conversation may appear once per
roster. Continuing every member of a fan-out is a round
(`docs/fan-out.md` § Recovery stays per member; a round addresses the set).

**No live steering.** No provider accepts input into a running turn: claude's
streaming input queues each message as a new turn — a multi-turn job in
disguise — and codex exec has no channel at all (`EVIDENCE.md`, 2026-07-28).
A supplement ("forgot to mention X") is a follow-up turn continuing the job. A
`steer` verb that only tells the caller so is rejected as surface the caller
learns and never reaches for (`EVIDENCE.md`, 2026-09-12).

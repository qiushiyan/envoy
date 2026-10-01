# Turn lifecycle and recovery

A turn is one provider process supervised to a terminal record. This doc is
why the lifecycle observes what it does, why recovery follows prompt state
alone, why session locks are never reclaimed, and how a finished turn's
conversation continues. `internal/runner` owns the lifecycle as a
single-threaded state machine; `internal/collect` owns the eligibility and
recovery reads.

## Process exit and stream EOF

A provider runs in its own process group, and process exit and stream EOF are
separate observations: a timed-out provider once left grandchildren holding
the stdio pipes, the stream-close event never fired, and the runner hung
(`EVIDENCE.md`, 2026-07-06). Stopping escalates exit → grace → SIGKILL over
the whole group, and the runner passes raw pipe fds rather than exec's managed
pipes, so waiting on the process observes its exit alone.

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

Every failure path in the engine routes to one of these, and recovery prose
prescribes nothing else (`prose.Recovery`). Re-sending a prompt the provider
already accepted duplicates work that may already have changed the tree — the
expensive mistake this rule exists to prevent.

## Session locks

One live turn per session id, including an id that only arrives mid-stream.
A lock is refused, never reclaimed: a dead runner can leave a live orphan
provider, and automatic stale takeover cannot be made race-free with a plain
lock file, so the engine only ever says no and points at the job owning the
session, to inspect first. Correctness of a shared conversation outranks
convenience. Locks live in `~/.local/state/envoy/locks/`, and a refusal
records the lock file's account of its holder
(`docs/records-and-collection.md`).

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

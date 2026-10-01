# Fan-out: several turns, one job

Several `--with` voices run several turns at once, each on its own prompt or
the job's default. The rule that keeps this from becoming a second engine:
**a fan-out is a supervisor over unchanged turns, not a new kind of turn.**
Members are ordinary turns — own session, own lock, own job directory, own
prompt state, own `result.md`, one level down in the group directory — so
every lifecycle invariant holds unmodified and `envoy collect <job>/<member>`
still works. The group adds only what the caller was otherwise doing by hand:
one process to wait on, one completion, one collect, one exit code.
`internal/fan` starts the members and reports them as a set, knowing nothing
about the turn lifecycle.

The fan-out cost no lifecycle change, which is the admission test any feature
must pass here: supervision and presentation, yes; new turn semantics, no
(`EVIDENCE.md`, 2026-07-26).

## `group.json` is a roster, never a mirror of member state

A member's status lives in that member's `meta.json` and nowhere else, so
manifest and member cannot drift; collect re-reads the members. The manifest
records only what the supervisor itself knows: the roster, whose names are the
member directories; the tree, cap and baseline the members share; and who
dispatched it and from which process, since those must be readable before any
member has a record (`docs/naming-and-storage.md` § Name holds).

## Recovery stays per member; a round addresses the set

Prompt state is per member: one `accepted` member licenses only a resume while
its `not_started` sibling licenses an identical retry, so there is no
group-wide retry, and every failure sentence points back at per-member
actions.

A follow-up **round** is not recovery. `--with @<fan-out>` alone continues
every member of a finished fan-out on one NEW prompt — a new fan-out over
ordinary turns, each member resolved exactly as a directly named
`@<fan-out>/<member>` would be and keeping its name, with sessions, cwd and
baseline read from the members' own records, so the caller re-decides nothing
per member. Agents hand-rolled this round a different way each time before it
existed (`EVIDENCE.md`, 2026-07-28).

- **Eligibility is judged for the whole set before anything is reserved:** a
  member still running or without a session refuses the round rather than
  being silently left out of it.
- **A session found held only at dispatch time** refuses that member alone,
  and the round reports partial. Reserving every session up front through a
  supervisor-held lock is rejected: it splits lock ownership between the
  runner and the supervisor for a case the caller can see in the collect block
  anyway.
- **Cherry-picking voices is a caller move:** one `--with
  @<fan-out>/<member>` each.

## Exit code 6, `partial`

A fan-out where one member answered and another timed out is not a failure
(results exist) and not a success (a member needs a decision). It earns a code
of its own rather than overloading one: reporting it as `4 timeout` invites
re-dispatching the whole fan and duplicating work already accepted. Where no
member returned a result, the most dispatch-side cause wins, since that is the
one fixed by changing the command instead of waiting (`job.ExitCodeForGroup`).

## The prompt attaches to the voice

A voice may carry its own prompt file (`--with codex=landscape.md`);
`--prompt-file` is the default for every voice without one, and optional once
each names its own. The fan-out's value was never equal prompts but one
dispatch, one wait, one collect. A caller giving each voice a different job
otherwise falls back to one brief that assigns the jobs by name — where the
voice assigned research did the critique instead — or to separate jobs and the
ledger the fan-out exists to absorb (`EVIDENCE.md`, 2026-09-14).

The first `=` splits: no provider, model, effort or job name contains one, a
prompt path may, and a job named by a directory path containing `=` is refused
by name rather than split into a job and a file nobody meant. Prompt selection
lives in the facade: every effective file is checked readable before the name
is reserved, and fan and runner receive a complete turn with no fallback rule
to learn.

Rejected shapes:

- **Pairing repeated `--prompt-file` flags with `--with` by position:** a
  swapped pair dispatches silently wrong, and the engine cannot refuse it.
- **Keying prompts by member address:** the engine allocates those, suffixes
  included.
- **A JSON seat (`--with '{"voice":…}'`),** proposed as the one encoding with
  no `=` ambiguity: a quoting tax on every dispatch for a path corner the
  engine's slug-sanitized store never produces.

What the rule admits next is bounded by the same test: a per-member cap would
be supervision; a per-member tree contradicts the one-tree roster; per-member
write intent stays refused.

## The fan-out directory holds no prompt

Each member archives the one it was sent. A group copy kept only while the
prompts match would add a second concept (does this group share a prompt?)
plus an equality rule. It follows that:

- a manifest that cannot be written stops the dispatch before any member
  starts, because a directory holding turns and no `group.json` reads as
  unstarted and is released;
- `pending` reports a roster member with no `meta.json` as needing attention,
  the roster being the only evidence that member was meant to run.

## Read-only by refusal

`--allow-write` is refused on a fan-out: members share one working tree, and
concurrent write turns overwrite each other. Parallel write work means a
worktree per turn, dispatched as separate jobs.

## Members run in one process

Each member runs its own event loop in the supervising process. The runner is
instance-clean — parameterized by its writers and job directory, with no
package-level state — which is what made the fan-out free. Spawning `envoy
run` subprocesses is rejected: the supervisor would become a second, drifting
copy of the CLI's argv and validation surface. The cost accepted in exchange
is blast radius, so a member panic is contained per member rather than taking
its siblings down.

## Member stdout is dropped, not interleaved

A single-turn block per member on one stdout would be unreadable for the agent
those blocks are written for, and nothing is lost: every coordinate they carry
is in the member's `meta.json`. Member stderr is labelled and passed through,
because warnings there are rare and load-bearing.

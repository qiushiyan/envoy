# Design — why envoy is shaped this way

CLAUDE.md and the code say what envoy does; this file records **why** — the
assumptions, rejected alternatives, and settled non-goals a future redesign
needs but can't see in the code. envoy descends from `sidekick-runtime` (a
Node engine living in the author's Claude Code skills); its lessons were
bought there and carry over undiminished.

**[EVIDENCE.md](EVIDENCE.md) is the log of what those lessons cost** — one
entry per incident or mining pass, in date order. Read it when a change would
revisit a settled question here; add to it when usage teaches something new.
This file states the position, that one records why it was paid for.

## Origin and the governing lesson

The lineage starts with a tmux-pane cross-reviewer that screen-scraped a
provider CLI, and two sessions of friction: completion watchers
false-positiving on input prompts, readiness checks timing out against splash
screens, the human hand-typing "seems it finished". The lesson that governs
everything here: **a screen is not an API.** Sessions are driven headless and
read back as data — JSON event streams and result envelopes, never scraped
output. Everything else in this design is downstream of that: durable files
as the return value, semantic events as the runner's currency, stdout as a
deliberate interface rather than a byproduct.

The Go rewrite (2026-07-25) had four reasons, in order: a single binary on
PATH instead of `node <path-to-script>`; a real provider seam instead of
conditionals scattered through one file; one owner for the meta schema so the
writer (turn) and reader (collect) cannot drift; and Go's stdlib handling the
process-lifecycle traps the JS closed by hand. **Zero third-party
dependencies** continues the predecessor's "Node builtins only" ethos — the
constraint is load-bearing: it keeps the whole program auditable in one
sitting and removes an entire class of maintenance.

## Division of labor

The engine is deterministic mechanism: one turn in, files out. It holds zero
judgment — no retries, no review loops, no gates, no provider selection, no
prompt templating. Callers (a human, an agent following a skill, a script)
own all judgment: when to dispatch, what the prompt contains, how to verify
what comes back, whether to resume or retry. This split is why the engine can
be small and why its callers can be smart.

**Fold-back trigger:** if the engine ever wants retries, review loops, or
unattended-resilience machinery, it has outgrown its purpose — that
intelligence belongs in a caller, not here. Don't grow the engine.

Two adjacent non-responsibilities, both settled: envoy is **not a job
manager** (the caller's background-task layer owns liveness and completion
notification; `pending` is only a recovery index over durable files after a
notification may have been missed) and **not a sandbox** (`--allow-write` is
intent, not enforcement; read-only is a prompt convention, and codex
sandboxing is left to the provider's own configuration — a derived read-only
sandbox once broke the calling session's own tooling).

## Commitments

- **Observed, never inferred.** The engine reports what it was asked
  (`model opus`, `(provider default)`) and what it observed (the provider's
  own init-event model lands in `providerReportedModel`; a codex thread id
  arrives mid-stream) — never what it concluded. Alias translation
  (`opus` → some concrete id) is rejected permanently: the mapping is the
  provider's, changes under the engine's feet, and the first disagreement is
  a silent model substitution. Same rule for effort: native provider
  vocabulary only, validated pre-spawn because the providers fail differently
  (claude silently degrades, codex burns a turn on an API 400). The same rule
  binds every sentence the engine says about itself: **an observation is
  pinned to the instant and the channel it was read on.** A stream read after
  teardown describes envoy's own cleanup, not the provider; raw bytes are not
  parseable events; and a store that could not be read is not an empty store,
  so discovery refuses instead of reporting a project with no jobs.
- **Durable artifacts over stdout — but stdout still programs the caller.**
  Files are authoritative; stdout is a convenience view of them. The
  convenience view is read by an agent, so startup lines, progress
  vocabulary, collection blocks, and recovery prose are prompts: give the
  reader facts, state, and one proven next action — never protocol trivia to
  reinterpret. `next:` lines exist so the caller never has to derive the next
  move from status codes.
- **Background is the default posture.** Collection is notification-driven;
  the process exiting is the completion signal. Callers can sample durable
  usage observations while a turn runs; sample age cannot establish a hang
  or completion (EVIDENCE.md, 2026-07-11). There is no watch command or
  `--detach` mode.
- **The name is the address.** The caller chooses the job's name before
  dispatch and collects by it, so nothing printed by a dispatch whose stdout
  the harness hides has to be read back. Two handoffs were tried and paid for
  first — "the newest job in the store" (rejected: name-ordered, printed
  before the first record, and racy against a second dispatch in the same
  project) and a coordinate file the dispatch wrote (retired: the path itself
  had to survive across the caller's shell calls, and the logs showed it did
  not — EVIDENCE.md, 2026-09-12). A directory is reserved by creating it, so
  creation is the collision check. A refusal that ran nothing gives the name
  back.
- **A name addresses its latest generation; a directory is an identity.**
  Callers do not invent names: the skills that drive the engine hand every
  session `review-r1` and `consult-r1`, so in a checkout that outlives one
  task, reuse is the normal case. The first rule was refuse-never-suffix, on
  the argument that a silent suffix leaves the caller collecting somebody
  else's job — and refusal did exactly that: a refusal whose exit code a
  pipe had swallowed went unseen, and `collect` by the name served the old
  job as `ok` (EVIDENCE.md, 2026-09-21). The name was already ambiguous
  across time; refusing only chose the stale reading. So a name whose job
  was **delivered** — every turn terminal and collected, the meaning
  `collectedAt` already had — passes to the next dispatch, which runs in
  `<name>+2`, and `collect` and `@<job>` resolve a name to its highest
  generation. `+` is outside the name pattern, so no caller can name a
  generation; its path reaches it. Rejected: rotating the old directory
  aside so the new job takes the bare path (every `resumedFrom` record and
  printed `resume:` command naming the old path would silently mean a
  different job); always passing the name on (a caller still waiting on a
  running or uncollected job would collect the newcomer's result, so that
  one case stays a refusal — the only collision a caller hears about); an
  age threshold for "stale" (judgment; delivery is an observation). Two
  edges follow from holding only what a collect could still deliver. A
  fan-out member refused before its first record — a session held at
  dispatch — has nothing to deliver, so once a sibling has a record it holds
  nothing; counting it would pin the name for good, since no collect stamps
  a member that has no record. And a store that exists but cannot be listed
  is an error, never generation zero: the bare name there could be an older
  job served as the latest.
- **Records hold facts; collect renders.** `meta.json` carries what the turn
  was dispatched with and what the engine observed, never a rendered command
  or prescription: a persisted command drifts with every wording or verb
  change and cannot be repaired without a compatibility layer, and a record
  from another schema version is refused by name instead of reinterpreted.
  Every `resume:`, retry and `next:` line is rendered at collect time from the
  fields plus the directory the record was read from, so the identical retry
  can repeat the dispatch it replaces — the same source conversation, the
  archived prompt, the spend cap — and the resume line can never disagree
  with what dispatch would accept. The archive is what was sent: `prompt.md`
  is written from the one read that feeds the provider's stdin, never from a
  second read of the caller's file. A record is also what makes a directory
  a job — `meta.json` for a turn, `group.json` for a fan-out — so discovery
  keys on records and a reserved name released without one was never a job.
- **A provider's transient error is an observation; only its verdict fails a
  turn.** codex emits a bare `error` event per reconnect attempt and then
  carries on; a driver that let the last one stand as the outcome recorded
  `failed` over a completed turn with a full result (EVIDENCE.md, 2026-08-28).
  `turn.failed` is the verdict. The reconnect events themselves are tallied
  (`connectionErrors` in meta, `provider stream:` in the diagnostic tier) and
  worded as what the provider said — never "offline", which the engine cannot
  know — and nothing acts on the tally: the cap does not pause, recovery still
  follows prompt state alone.
- **Recovery reasons from evidence, not silence.** Prompt state has three
  values with distinct actions: `accepted` → resume, never redispatch (work
  may exist); `not_started` → one identical retry is safe; `unknown` →
  absence of output is not proof of no work. Every failure path in the
  engine routes to one of these; the timeout is a wall-clock safety cap and
  deliberately not a stall detector, because healthy deep work and a hang
  are indistinguishable from outside — the engine never *acts* on quiet, but a
  capped turn's envelope *reports* the stream it saw, since a cap alone reads
  the same either way. Eligibility to continue a conversation likewise has
  exactly one definition, consulted by every surface that advertises or
  dispatches one, so collect can never offer a continuation dispatch refuses.
- **Locks are refused, never reclaimed.** A dead runner can leave a live
  orphan provider, and automatic stale takeover cannot be made race-free
  with a plain lock file — so the engine only ever says no and points at the
  job to inspect. Correctness of a shared conversation outranks convenience.

## Provider launchers

A driver owns provider-native arguments and environment additions; the runner
prepends the command from the dispatch environment. Account selection and
child environment changes belong to that launcher. Envoy never detects a
launcher or falls back when one fails: fallback could spend on the very account
the caller meant to avoid. Process groups, pipes and session-id locks supervise
the resulting command unchanged.

The prefix is literal whitespace-separated argv, with complex setup left to a
wrapper executable. `commandPrefix` records it as an optional observation;
continuations and retries resolve the current environment, so account choice
stays with the launcher. `README.md` § Running providers through a launcher
owns the configuration and recorded-argv contract. After a successful spawn,
an exit without provider evidence leaves prompt state unknown: silence cannot
prove that no work ran, even when a launcher's stderr explains its refusal.

Claude transcript recovery is best-effort in envoy's own config directory.
A launcher may choose a child directory envoy cannot know; a shared projects
store can preserve recovery, but the engine cannot infer it from command text.

## Fan-out: several turns, one job

Several `--with` voices run several turns at once, each on its own prompt or
the job's default. The design rule that keeps it from becoming a second
engine: **a fan-out is a supervisor over unchanged turns, not a new kind of
turn.** Members are ordinary turns — own
session, own lock, own job dir, own prompt state, own `result.md`, one level
down in the group dir — so every lifecycle invariant above holds unmodified
and `envoy collect <job>/<member>` still works. What the group adds is only what
the caller was otherwise doing by hand: one process to wait on, one completion,
one collect, one exit code.

Consequences that are load-bearing, not incidental:

- **`group.json` is a roster of names, never a mirror of member state.**
  A member's status lives in that member's `meta.json` and nowhere else, so the
  two cannot drift. Collect re-reads the members; the manifest records only
  what the supervisor itself knows: the roster, whose names are the member
  directories, and the tree, cap and anchor the members share.
- **Recovery stays per member; a round addresses the set.** Prompt state is
  per member: one `accepted` member licenses only a resume while its
  `not_started` sibling licenses an identical retry, so there is no group-wide
  retry and every failure sentence points back at per-member actions. A
  *follow-up round* is not recovery: `--with @<fan-out>` alone re-dispatches
  every member of a finished fan-out as a continued turn on one NEW prompt — a
  new fan-out over ordinary turns, each member resolved exactly as a directly
  named `@<fan-out>/<member>` would be, keeping its name, with the sessions,
  cwd and baseline read from the members' own records so the caller re-decides
  nothing per member. Eligibility is judged for the whole set before anything
  is reserved: a member still running or without a session refuses the round
  rather than being silently left out of it. A session found held only at
  dispatch time refuses that member alone and the round reports partial;
  reserving every session up front through a supervisor-held lock is
  rejected, because it splits lock ownership between the runner and the
  supervisor for a case the caller can see in the collect block anyway. Cherry-picking voices is a
  caller move, one `--with @<fan-out>/<member>` each. (The round was paid for
  before it was built: agents hand-rolled it three different ways in three
  days — EVIDENCE.md, 2026-07-28.)
- **Exit code 6 (`partial`) earned a new code** rather than overloading an
  existing one. A fan-out where one member answered and one timed out is not a
  failure (results exist) and not a success (a member needs a decision);
  reporting it as `4 timeout` invites re-dispatching the whole fan and
  duplicating work already accepted. Where no member returned a result, the
  most dispatch-side cause wins, since that is the one fixed by changing the
  command instead of waiting.
- **The prompt attaches to the voice.** A voice may carry its own prompt file
  (`--with codex=landscape.md`); `--prompt-file` is the default for every
  voice without one and is optional once each names its own. The fan-out's
  value was never equal prompts but one dispatch, one wait, one collect, and a
  caller giving each voice a different job otherwise falls back to one brief
  that assigns the jobs by name — where the voice assigned research did the
  critique instead — or to separate jobs and the ledger the fan-out exists to
  absorb (EVIDENCE.md, 2026-09-14). The first `=` splits: no provider, model, effort
  or job name contains one, a prompt path may, and a job named by a directory
  path containing `=` is refused by name rather than split into a job and a
  file nobody meant. Rejected shapes: pairing repeated `--prompt-file` flags
  with `--with` by position (a swapped pair dispatches silently wrong and the
  engine cannot refuse it); keying prompts by member address (the engine
  allocates those, suffixes included); a JSON seat (`--with '{"voice":…}'`),
  proposed as the one encoding with no `=` ambiguity — a quoting tax on every
  dispatch for a path corner the engine's slug-sanitized store never produces.
  Prompt selection lives in the facade: every effective file is checked
  readable before the name is reserved, and fan and runner receive a complete
  turn with no fallback rule to learn. What the rule admits next is bounded
  by the same test — a per-member cap would be supervision, a per-member tree
  contradicts the one-tree roster, per-member write intent stays refused.
- **The fan-out directory holds no prompt.** Each member archives the one it
  was sent, and a group copy kept only while the prompts match would add a
  second concept (does this group share a prompt?) plus an equality rule. Two
  consequences: a manifest that cannot be written stops the dispatch before
  any member starts, because a directory holding turns and no `group.json`
  reads as unstarted and is released; and `pending` reports a roster member
  with no `meta.json` as needing attention, the roster being the only
  evidence that member was meant to run.
- **Read-only by refusal.** `--allow-write` is rejected on a fan-out: members
  share one working tree and concurrent write turns overwrite each other.
  Parallel write work means a worktree per turn, dispatched as separate turns.
- **Members run in one process, on N event loops.** The runner was already
  parameterized by its writers and job dir, which is what made this free; the
  alternative — spawning `envoy run` subprocesses — was rejected because the
  supervisor would become a second, drifting copy of the CLI's argv and
  validation surface. The cost accepted in exchange is blast radius, so a
  member panic is contained per member rather than taking its siblings down.
- **Member stdout is dropped, not interleaved.** N single-turn blocks on one
  stdout would be unreadable for the agent those blocks are written for, and
  nothing is lost: every coordinate they carry is in the member's `meta.json`.
  Member stderr is labelled and passed through, because warnings there are rare
  and load-bearing.

## Collection: delivery, not display

`collectedAt` means one thing: *the return value reached a caller*. The stamp
marks a delivered deliverable — an ok turn's printed result body, or a non-ok
turn's full diagnostic block, whose status and recovery are its result — and
is never set by `--status-only` or `pending`, which read coordinates without
delivering anything, nor for an ok turn whose `result.md` would not read: in
both cases the result stays owed and pending keeps listing the job.

The two selection flags exist because the block is read by an agent whose
context the result body and the status preamble compete for. They select
sections, never soften the contract: `--result-only` on anything
other than an ok turn prints the full block, because a non-ok turn's status
and next action *are* its result, and handing back silence in their place
would manufacture a payload that does not exist.

**The block itself is tiered on a narrower question: did the payload land?**
Settings, token counts, context usage, prompt-state evidence, result kind and
log paths belong to the diagnostic tier. A turn that reports ok *and* whose
`result.md` reads holds that tier back and prints what remains to act on
(coordinate, status, duration, follow-up commands, payload, next action);
everything else prints the whole preamble, and
`--status-only` prints it on demand. Note the asymmetry with the stamp above: a
non-ok turn's block *is* a delivered deliverable and gets stamped, yet it still
prints in full, because what it delivers is the diagnosis. Keying the tier on
status instead was tried and is wrong at both edges — the undelivered ok turn
above is sent to `raw.log` by its own next line, and `--status-only` delivers
nothing by construction. Nothing is hidden either way, which is what makes the
tiering safe: **stdout is the convenience view and the files are the truth.**

Several temptations were rejected, each a rule that would fire on the wrong
cases. Gating the individual log paths on which recovery branch names them:
the three streams are one affordance, and which one a branch emphasises is not
eligibility. Printing the settings line when the provider's reported model
differs from the requested one, which *looks* like reporting a substitution —
but `opus` against `claude-opus-5` is the provider's own alias resolution, the
mapping this engine refuses to own, so inequality cannot tell resolution from
substitution and the "exception" fires on the most ordinary dispatch there is.
And keeping the bare `session:` line beside the commands that already spell the
id out: three copies of one identifier is three invitations to hand-assemble a
follow-up instead of running the one carrying the turn's cwd and write intent.

## Primary-turn usage

`meta.json.usage` lets a caller read Claude's primary-turn measurements without
interpreting the provider stream. The Claude driver owns attribution,
deduplication and reconciliation; `KindUsage` carries independent snapshots
that the runner atomically publishes as they arrive, without waiting for a
heartbeat. `internal/job/usage.go` owns the record, and
`internal/provider/claude_usage.go` owns the accounting. The measurement adds
no lifecycle decisions: callers own any policy about context pressure.

The scope is one job's primary turn. A continuation starts fresh counters and
a fresh peak, even though its first request includes earlier conversation
context. Fan-out members keep their own measurements. Codex leaves `usage`
absent; absence in an older record likewise means unavailable.

- **Context and freshness.** `latestContextTokens` is the last valid
  response's input plus cache-read plus cache-creation tokens.
  `peakContextTokens` is the largest such sample in this turn; compaction can
  lower latest without lowering peak. `sampledAt` is Envoy's receipt time for
  that sample. Neither count includes subsequent generated output or tool
  results, so it cannot predict the next request's size.
- **Response accounting.** `responses` counts distinct attributed primary
  message IDs, even when a response lacks usable input usage. Repeated IDs
  change nothing, including the sample time. Non-null `parent_tool_use_id`
  excludes a record before deduplication. `inputTokens`, `cacheReadInputTokens`
  and `cacheCreationInputTokens` accumulate valid input measurements;
  `outputTokens` stays null until terminal reconciliation accepts a complete
  output total. Missing or invalid measurements remain unknown, never zero.
- **Attribution.** `attribution` starts `unknown`; the reported session model
  establishes `complete` attribution until a response has missing or differing
  model evidence. Such a response leaves the last valid sample intact and
  marks attribution `incomplete`; `unexpectedModel` holds the last differing
  model. Later valid samples advance the measurement, but the issue persists.
  The comparison uses provider-reported models, never a requested alias.
- **Window and terminal totals.** `contextWindowTokens` stays null until the
  terminal record gives a positive window for the session's reported model.
  It is the only figure taken from `modelUsage`, whose totals include work
  outside the primary loop. `terminalTokens` preserves the terminal
  `result.usage` fields as `input`, `cacheRead`, `cacheCreation` and `output`,
  with missing or invalid fields null. Complete totals are accepted only
  when they agree with observed inputs and attribution permits reconciliation.
  Missing fields, budget stops, zeroed crash usage or contradictory totals
  preserve observed inputs and leave full output unknown. Terminal totals
  cannot reconstruct a missing context sample or peak.
- **Finality and completeness.** `state` is `unmeasured` until a valid context
  sample, then `live`. Once `final` is true, it is `settled` when there are no
  issues, otherwise `incomplete`. `issues` retains distinct evidence gaps;
  live measurements can already carry issues. Finality is independent of job
  success and process exit: a failed turn can have complete usage, and usage
  can settle while process cleanup continues. A missing window leaves the
  block incomplete even when output is known.

Runner finalization without a terminal record retains the live figures and
adds `missing_terminal`. An Envoy process killed before finalization can leave
a non-final snapshot indefinitely; metadata alone cannot prove liveness.
Collection marks unfinished usage incomplete when it proves abandonment,
preserving any measurement already finalized by a terminal record.

Accounting does expected O(1) work per record with O(distinct message IDs)
deduplication memory; snapshots have fixed structure and a finite issue
vocabulary. It does not reparse stream history for each update. The existing
`tokens` block retains its provider-reported meaning and finalization writer.

Optional observations retain meta schema **9**: existing fields and
continuation eligibility keep their meaning, and readers treat absent usage
as unavailable. Bumping the schema would refuse compatible jobs and force
callers to change their schema pins. `TestUsageSchemaCompatibility` pins this
boundary; the engine version identifies feature availability. The captured
evidence behind the accounting lives in `EVIDENCE.md`, 2026-09-14.

## Storage

Jobs live in one central store, `~/.local/state/envoy/jobs/<slug>/<name>/`,
never in the project tree (the predecessor's repo-local `.sidekick/` dirs are
deliberately gone — they polluted every repo with runtime state). The slug is
`basename-hash8`: the basename for humans scanning the store, the hash of the
symlink-resolved path for uniqueness, the git root as anchor so a dispatch
from a subdirectory belongs to the project. The store is an implementation
detail by contract: the caller names the job, `run`, `collect` and `pending`
re-derive the project's store from the directory they run in, and no caller
constructs the path — a name is one path segment, and anything starting with
`/`, `.` or `~` is a directory taken as given. A name's later generations
sit beside the first as `<name>+N`. Creating the job dir is the
reservation — `Mkdir`, not stat-then-create — because two turns dispatched in
the same second once shared one (EVIDENCE.md, 2026-07-27).

## Deliberately not built

- No daemon, status command, cancel service, or job listing — the caller's
  background-task layer is the live-job layer, and the caller named the job
  it wants. A listing was built when callers rebuilt stamp-named paths by hand
  (EVIDENCE.md, 2026-07-31) and removed once names were the caller's own;
  `pending` is the one index, and it is recovery, not discovery.
- No alias translation, effort aliases, model fallbacks, or provider
  auto-selection.
- No group-wide retry, and no partial resume of a fan-out: recovery is per
  member, and a round is refused when a member cannot continue.
- No live steering of a running turn: no provider accepts input into one —
  claude's streaming input queues a NEW turn (a multi-turn job in disguise),
  codex exec has no channel at all (verified research: EVIDENCE.md,
  2026-07-28). A supplement is a follow-up turn, `--with @<job>`; a `steer`
  command saying so is surface the caller learns and never reaches for
  (EVIDENCE.md, 2026-09-12).
- No sandbox flag for codex, ever; no permission machinery beyond claude's
  own `--permission-mode`.
- No activity-based hang detector; no timeout that resets on output.
- No prompt templating; prompts arrive as files, whole — one file per voice
  when the voices' questions differ.
- No Windows support; no migration from sidekick-era state.

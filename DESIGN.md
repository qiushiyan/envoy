# Design — why envoy is shaped this way

CLAUDE.md and the code say what envoy does; this file records **why** — the
assumptions, rejected alternatives, and paid-for lessons a future redesign
needs but can't see in the code. Update the evidence log when usage teaches
something new. envoy descends from `sidekick-runtime` (a Node engine living
in the author's Claude Code skills); the lessons below were bought there and
carry over undiminished.

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
sandboxing is governed by `~/.codex/config.toml` alone — a derived read-only
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
  (claude silently degrades, codex burns a turn on an API 400).
- **Durable artifacts over stdout — but stdout still programs the caller.**
  Files are authoritative; stdout is a convenience view of them. The
  convenience view is read by an agent, so startup lines, progress
  vocabulary, collection blocks, and recovery prose are prompts: give the
  reader facts, state, and one proven next action — never protocol trivia to
  reinterpret. `next:` lines exist so the caller never has to derive the next
  move from status codes.
- **Background is the default posture.** Collection is notification-driven;
  polling a live job is a smell. The one sanctioned read after dispatch is
  the startup coordinate block; `watch:` is observation, never a completion
  or acceptance signal (a lesson paid for in a real incident — see the log).
- **Recovery reasons from evidence, not silence.** Prompt state has three
  values with distinct actions: `accepted` → resume, never redispatch (work
  may exist); `not_started` → one identical retry is safe; `unknown` →
  absence of output is not proof of no work. Every failure path in the
  engine routes to one of these; the timeout is a wall-clock safety cap and
  deliberately not a stall detector, because healthy deep work and a hang
  are indistinguishable from outside.
- **Locks are refused, never reclaimed.** A dead runner can leave a live
  orphan provider, and automatic stale takeover cannot be made race-free
  with a plain lock file — so the engine only ever says no and points at the
  job to inspect. Correctness of a shared conversation outranks convenience.

## Fan-out: several turns, one job

`envoy fan` sends one prompt to several turns at once. The design rule that
keeps it from becoming a second engine: **a fan-out is a supervisor over
unchanged turns, not a new kind of turn.** Members are ordinary turns — own
session, own lock, own job dir, own prompt state, own `result.md`, one level
down in the group dir — so every lifecycle invariant above holds unmodified
and `envoy collect <member-dir>` still works. What the group adds is only what
the caller was otherwise doing by hand: one process to wait on, one completion,
one collect, one exit code.

Consequences that are load-bearing, not incidental:

- **`group.json` is a roster of coordinates, never a mirror of member state.**
  A member's status lives in that member's `meta.json` and nowhere else, so the
  two cannot drift. Collect re-reads the members; the manifest records only
  what the supervisor itself knows (who was dispatched, where, when, and — for
  a resumed fan-out — which fan-out its sessions continue).
- **Recovery stays per member; a round addresses the set.** Prompt state is
  per member: one `accepted` member licenses only a resume while its
  `not_started` sibling licenses an identical retry, so there is no group-wide
  retry and every failure sentence points back at per-member actions. A
  *follow-up round* is not recovery: `fan --resume-from` re-dispatches every
  member of a finished fan-out as a resumed turn on one NEW prompt — a new
  fan-out over ordinary turns, with the roster, sessions, cwd, and baseline
  read from the original's own records so the caller re-decides nothing per
  member. The set continues whole or is refused: a member still running or
  without a session blocks the round rather than being silently left out of
  it. Cherry-picking voices stays a caller move, via the per-member
  `turn --resume` commands collect prints. (The feature was paid for before it
  was built: agents hand-rolled the round three different ways in three days —
  see the log, 2026-07-28.)
- **Exit code 6 (`partial`) earned a new code** rather than overloading an
  existing one. A fan-out where one member answered and one timed out is not a
  failure (results exist) and not a success (a member needs a decision);
  reporting it as `4 timeout` invites re-dispatching the whole fan and
  duplicating work already accepted. Where no member returned a result, the
  most dispatch-side cause wins, since that is the one fixed by changing the
  command instead of waiting.
- **Read-only by refusal.** `--allow-write` is rejected on a fan-out: members
  share one working tree and concurrent write turns overwrite each other.
  Parallel write work means a worktree per turn, dispatched as separate turns.
- **Members run in one process, on N event loops.** The runner was already
  parameterized by its writers and out-dir, which is what made this free; the
  alternative — spawning `envoy turn` subprocesses — was rejected because the
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
both cases the result stays owed and pending keeps listing the job. The two selection flags exist because the block is
read by an agent whose context the result body and the status preamble
compete for (the logs showed nearly every collect piped through `sed`/`head`).
They select sections, never soften the contract: `--result-only` on anything
other than an ok turn prints the full block, because a non-ok turn's status
and next action *are* its result, and handing back silence in their place
would manufacture a payload that does not exist.

## Storage

Jobs live in one central store, `~/.local/state/envoy/jobs/<slug>/`, never in
the project tree (the predecessor's repo-local `.sidekick/` dirs are
deliberately gone — they polluted every repo with runtime state). The slug is
`basename-hash8`: the basename for humans scanning the store, the hash of the
symlink-resolved path for uniqueness, the git root as anchor so a dispatch
from a subdirectory belongs to the project. The store is an implementation
detail by contract: dispatch prints the out-dir, collect and pending
re-derive it from cwd, and no caller constructs the path. Creating a job dir
is atomic — `Mkdir`, not stat-then-create — because two turns dispatched in
the same second once shared one (see the log, 2026-07-27).

## Deliberately not built

- No daemon, status command, cancel service, or job listing — the caller's
  background-task layer is the live-job layer.
- No alias translation, effort aliases, model fallbacks, or provider
  auto-selection.
- No group-wide retry, and no partial resume of a fan-out: recovery is per
  member, and a round continues the whole set or is refused.
- No live steering of a running turn: no provider accepts input into one —
  claude's streaming input queues a NEW turn (a multi-turn job in disguise),
  codex exec has no channel at all. `envoy steer` answers with the follow-up
  command instead; the verified research is in the log (2026-07-28).
- No sandbox flag for codex, ever; no permission machinery beyond claude's
  own `--permission-mode`.
- No activity-based hang detector; no timeout that resets on output.
- No prompt templating; prompts arrive as files, whole.
- No Windows support; no migration from sidekick-era state.

## Evidence log

Engine-relevant history, distilled from the predecessor and continued here.

- **2026-07-03 — engine born** from the `/pair-coding` screen-scraping
  postmortem: headless turns, files as return values.
- **2026-07-06 — the close-hang.** A timed-out provider left grandchildren
  holding the stdio pipes; the stream-close event never fired and the runner
  hung. Fix: process exit and stream EOF are separate observations, with an
  exit → grace → SIGKILL escalation. This is why the Go port passes raw pipe
  fds instead of exec's managed pipes.
- **2026-07-10 — lifecycle hardening.** Provider turns moved into their own
  process group; atomic metadata gained runner/provider PIDs and prompt
  state; collection became idempotent; pending discovery learned to
  distinguish orphaned from abandoned work.
- **2026-07-11 — the buffered-watch incident.** A consult burned 20 minutes
  on a false hang diagnosis because `--output-format json` emitted one
  buffered result while `tail -f raw.log` implied live evidence. Fixes that
  stand: realtime `stream-json`, runner-owned heartbeats in `progress.log`,
  watch demoted to observation, transcript-evidence recovery for the window
  before the first stream event. Two boundary races closed the same day:
  stale locks are never auto-reclaimed over a possible orphan, and an
  observed terminal envelope beats a cap that fires during cleanup.
- **2026-07-25 — Go port (this repo).** Contract preserved against the JS
  reference; the old integration scenarios re-expressed against the built
  binary, race-instrumented. Same day: jobs moved to the central hidden
  store, and the first dogfood dispatch (an opus review of this repo)
  exercised the claude driver, central storage, and the coordinate block on
  a real provider.
- **2026-07-25 — resolved-model observation.** `--model opus` provably ran
  `claude-opus-5`, but only `raw.log` knew: the alias is a rolling "latest"
  pointer and the engine echoed only the request. Added
  `providerReportedModel` — the provider's own init-event statement,
  surfaced in progress and collect. The boundary it sharpened: *observed* is
  recordable, *inferred* stays forbidden.
- **2026-07-26 — the caller-facing prose became a package.** A prompt-
  engineering pass found the same recovery rule written twelve ways across
  the runner, both drivers, and collect, already drifting; a resume
  *fragment* that made the caller assemble a command and silently dropped
  the original turn's `--allow-write`; status words with no gloss; and
  dispatch prose naming one specific caller harness. `internal/steer` now
  owns every sentence: drivers report causes, steer prescribes. The rule
  that generalizes — **a recovery line may prescribe only what the engine
  observed, and must hand over a runnable command, not a fragment.**
- **2026-07-26 — fan-out.** Real use wanted one prompt on two models ~10% of
  the time, and doing it with two dispatches made the caller hold a two-job
  ledger and answer a question the engine should have absorbed ("one voice is
  back, synthesize now or wait?"). `envoy fan` collapses it to one dispatch,
  one completion, one collect. It cost no lifecycle change — the runner was
  already instance-clean — which is exactly the test of whether a feature
  belongs in this engine: *supervision and presentation, yes; new turn
  semantics, no.*
- **2026-07-26 — the stale-binary dispatch.** The skills learned `envoy fan`
  hours before the binary on PATH did; the dispatch failed and the calling
  agent silently fell back to two separate turns — the exact two-job ledger
  fan exists to absorb. No engine change: the lesson is operational. A
  contract taught to callers ships when the installed binary does, so
  `make install` comes before the skill edit, not after the next incident.
- **2026-07-27 — the same-second collision.** Two round-2 resumes dispatched
  with `&` in one command started in the same second, derived the same
  stamp+label job dir, and ran in it together: both runners overwrote one
  `meta.json` and `result.md`, and one member's answer survived only in
  `last-message.txt`. Root cause: stat-then-`MkdirAll`, where `MkdirAll`
  succeeds on a dir that already exists. Fix: `Mkdir` with a random suffix on
  `EEXIST` — creation itself is the collision check, and a concurrency test
  races eight same-second dispatches to keep it that way.
- **2026-07-28 — four days of dogfood logs read back.** All 37 archived turns
  had ended ok; every friction lived in how agents drove the CLI, not in the
  lifecycle. Three findings, each paid for in real time: the round-2-after-fan
  pattern was hand-rolled three ways in three days (`;`-chained resumes that
  silently serialized, `&`-chained ones that hit the collision above, then two
  background tasks with hand-invented labels) → `fan --resume-from`; agents
  trimmed nearly every collect through `sed`/`head` because the result body
  and the preamble compete for caller context → `collect --result-only` /
  `--status-only`; the group watch command shipped its glob inside shell
  quotes and could never run as printed → member paths spelled out, pinned by
  an exact-line test. The lesson that generalizes: **the engine's own job
  store is its usage lab — read it before inventing features, and after
  shipping them.** The same-day review round then caught the new features'
  own gaps, each fixed against a red test: a resumed round could dispatch
  partially when a member's session was held by another live turn (the round
  now reserves every member's session before any turn spawns), "resumable"
  had grown two contradictory definitions across collect and the facade
  (collapsed into one typed inspection, blockers worded by steer), and
  collect stamped `collectedAt` even when an ok turn's result body never
  reached the caller (the stamp now follows delivery).
- **2026-07-28 — steer, and the live-input research it banked.** "I forgot to
  mention X" wanted a supplement delivered into a running job. Verified live
  (claude 2.1.220, codex 0.144.6): claude's `--input-format stream-json`
  accepts further user messages but **queues each as its own turn** — the
  in-flight turn never sees it — one result envelope per message, delivery
  ack via `--replay-user-messages`, input shape undocumented; and when stdin
  EOF arrives after an idle result (the exact teardown order steering needs),
  the CLI lingers for minutes, while EOF before work exits in seconds. codex
  exec reads stdin once at dispatch and has no channel at all; injection
  lives only in experimental servers. So "steering" a live turn is really
  scheduling a second turn, and building delivery would make the job
  multi-turn — per-message prompt state, aggregate statuses, kill-based
  teardown for the linger; the meta schema's singular
  `promptState`/`providerTerminalAt` stop being honest. A design-blind codex
  consult reached the same verdict and named the admission test this fails:
  fan cost zero lifecycle change, this reshapes driver, runner, schema, and
  collect. Shipped instead: `envoy steer` as pure state inspection — always
  "not delivered", plus the one runnable follow-up with the supplement file
  filled into the prompt slot (the single place the placeholder closes), per
  member on a fan-out. `internal/steer` was renamed `internal/prose` so the
  command could take the name. If steer refusals ever pile up in the job
  store, that is the usage-lab evidence an opt-in streaming turn would need
  to justify its cost.
- **2026-07-29 — cross-phase continuation: `turn --resume-from`,
  `fan --with-from`.** Real usage runs consult at the top of a host session
  and review at the bottom, and the consult session — the voice's whole
  built mental model of the plan — died at the skill boundary: every review
  dispatched cold and re-derived its context from scratch. The primitives
  for continuing a conversation existed (`turn --resume`,
  `fan --resume-from`), but not the shape that workflow needs: a follow-up
  anchored on a *job dir* hours later, and a fan whose members mix one
  continued conversation with cold ones. Two additions, no new turn
  semantics (the fan admission test passes): `turn --resume-from <job-dir>`
  reads the session and unspecified settings from the job's records — the
  job dir is the correct anchor because a resumed claude conversation
  continues under a fresh id, so a remembered session id silently forks the
  conversation at a stale point, while the records always name the current
  head — and `fan --with-from <job-dir>` makes one member of a fresh roster
  continue a finished job's session (`fan.Member.Resume` already existed;
  only the spelling was missing). Both route through the one typed
  inspection collect's resume line uses, so dispatch and collect cannot
  disagree about which jobs may continue. Decisions banked: the timeout
  never inherits (the cap is phase policy — a 30-minute consult continues
  into a 60-minute review); write intent inherits and can only widen (a
  follow-up must not silently go read-only); the provider is not
  overridable (a conversation cannot change model family — carrying context
  to another family is prompt authorship, per the no-templating non-goal);
  a warm reviewer is a *follow-through* check, not an independent one — the
  mixed roster exists precisely so callers can put a cold voice beside it,
  and that judgment stays with callers. Deliberately not built: job
  discovery ("the newest consult for this project") — the retained
  coordinate stays the out-dir printed at dispatch, and a listing command
  waits for usage-lab evidence of callers actually fumbling to find old
  jobs. The same-day review round caught the feature's own gaps, each
  pinned red before its fix: collect's turn-level resume line advertised a
  continuation `--resume-from` would refuse (eligibility now has the one
  definition, `memberResumeBlocker`, consulted by every surface that
  advertises or dispatches one — the round-2 pass caught the third
  consumer, the fan redirect for a single-turn dir, still bypassing it —
  so a running turn's session prints without its resume command
  everywhere); `--with-from` silently
  narrowed a write-recorded source (now refused with the single-turn
  alternative, which keeps the intent) and dropped a warm source's recorded
  baseline (now inherited like cwd, agreeing across sources or refused);
  and a lone invalid `--with-from` was redirected to a `turn --resume-from`
  that could only fail identically (inspection now precedes the redirect).
  One finding was rebutted: the resumed round's dispatch block keeps its
  single group-level `resumed-from:` line instead of a per-member suffix —
  the report-once precedent that already governs the group baseline.

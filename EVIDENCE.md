# Evidence log — what usage taught, and what it cost

Engine-relevant history, distilled from the predecessor and continued here.
[DESIGN.md](DESIGN.md) holds the design these lessons produced; this file holds
the receipts. Read it when a change would revisit a settled question — the
entry usually says what it already cost to learn.

One incident or one mining pass per entry: what the logs showed, what it cost,
and the lesson that survived. A few lines each. An entry that has grown into a
session report has drifted off this altitude and belongs back at it — the
detail lives in the job store and the transcripts, which is where a later pass
will look anyway.

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
- **2026-07-28 — four days of dogfood logs read back.** All 37 turns had ended
  ok; every friction lived in how agents drove the CLI, not in the lifecycle.
  Three affordances the logs paid for: `fan --resume-from` (the round-2-after-
  fan pattern had been hand-rolled three ways in three days, one of them the
  collision above), `collect --result-only` / `--status-only` (nearly every
  collect was being trimmed through `sed`/`head`, because the result body and
  the preamble compete for the caller's context), and a group watch command
  whose glob sat inside shell quotes and could never run as printed. The lesson
  that generalizes: **the engine's own job store is its usage lab — read it
  before inventing features, and after shipping them.** The same-day review
  round caught the new features' own gaps, each pinned by a red test: a round
  that could dispatch partially when a member's session was held elsewhere,
  two contradictory definitions of "resumable", and `collectedAt` stamped over
  a result that never reached the caller.
- **2026-07-28 — steer, and the live-input research it banked.** Verified live
  (claude 2.1.220, codex 0.144.6): claude's `--input-format stream-json` accepts
  further user messages but **queues each as its own turn**, so the in-flight
  turn never sees it, and stdin EOF arriving after an idle result makes the CLI
  linger for minutes; codex exec reads stdin once at dispatch and has no channel
  at all. "Steering" a live turn is therefore scheduling a second turn, and
  delivering one would make the job multi-turn — per-message prompt state,
  aggregate statuses, kill-based teardown — at which point the schema's singular
  `promptState`/`providerTerminalAt` stop being honest. It fails the admission
  test fan passed: fan cost zero lifecycle change, this reshapes driver, runner,
  schema, and collect. Shipped instead: `envoy steer` as pure state inspection,
  always "not delivered" plus the one runnable follow-up. If steer refusals ever
  pile up in the store, that is the evidence an opt-in streaming turn would need.
- **2026-07-29 — cross-phase continuation: `turn --resume-from`,
  `fan --with-from`.** Consult ran at the top of a host session and review at
  the bottom, and the consult session — the voice's whole built model of the
  plan — died at the skill boundary: every review dispatched cold and re-derived
  its context. The primitives existed; the shape did not — a follow-up anchored
  on a *job dir* hours later, and a fan whose members mix one continued
  conversation with cold ones. The job dir is the right anchor because a resumed
  claude conversation continues under a fresh id: a remembered session id
  silently forks the conversation at a stale point, while the records always
  name the current head. Settled in the same pass: the cap never inherits (it is
  phase policy — a 30-minute consult continues into a 60-minute review), write
  intent inherits and may only widen, and the provider cannot change (carrying
  context to another family is prompt authorship, not continuation). Deferred:
  job discovery, until usage-lab evidence showed callers actually fumbling for
  old jobs — which arrived on 07-31.
- **2026-07-31 — three days of logs: the cap that lied, and the coordinate
  callers kept rebuilding.** Window `2026-07-29` → `2026-07-31T13:39`: 78 turns
  in the store, 145 envoy invocations across 24 sessions. Read with the caveat
  that the calling skills changed underneath the window, so caller-side friction
  dates to a skill version, not only to the engine.

  **The cap's envelope contradicted the job's own record.** Both timeouts in the
  window were the same shape — the provider announced its thread, then streamed
  nothing for the whole cap, 60 minutes once and 30 the next day — while the
  envelope said only that reaching the cap "is not evidence the provider hung".
  True of the mechanism, wrong for the run: one caller diagnosed the stall by
  hand from `stderr.log`, and the next believed the envelope and wrote a "finish
  what you started" follow-up into a session that had produced nothing. A capped
  turn now reports the stream it observed. The cap's *behavior* is unchanged —
  still a wall-clock deadline, never a stall detector. **Not acting on an
  observation is not a reason to withhold it.**

  **`envoy jobs`, which retired a settled non-goal.** Callers rebuilt job paths
  by hand in 22 of 136 envoy shell calls: five guesses at the stamp-and-label
  dir, all five naming a directory that did not exist (the stamp is the dispatch
  second, which nothing but the dispatch knows), and ten reaches into the host
  harness's private task file to grep back the `out-dir:` envoy had already
  printed. Also found: bare `envoy collect` already meant "this project's newest
  job" and was used that way zero times in 145 invocations — an affordance the
  caller docs never taught, fixed in the skill rather than the engine.

  **The review round is the other half of the lesson.** A cold reviewer found
  both new sentences overclaiming in precisely the way this pass was fixing: the
  cap read its stream *after* teardown, so a provider that emitted once while
  being killed was reported as "quiet for 0s" when it had been silent the whole
  cap — the engine describing its own cleanup back as provider work; and
  "streamed nothing" conflated raw bytes with parseable events. It also found
  `jobs` and `pending` reporting an unreadable or mistyped store as an empty one,
  and, in round two, that the first fix for that had been layered beside the old
  path rather than replacing it. **A sentence reporting an observation must be
  pinned to the instant and the channel it was observed on.** Recounting the
  window at the reviewer's insistence corrected several figures — the first
  count had measured rows mentioning envoy, not invocations, over a window that
  started a day late.
- **2026-08-02 — the preamble callers kept `sed`-ing past.** Window
  `2026-07-25` → `2026-08-01`: 158 `envoy collect` invocations across 40
  sessions (137 default block, 21 with a selection flag). Thirty-three piped the
  output into `head`/`tail`/`sed` or redirected it to a file, and fourteen of
  those jumped straight to the payload — `sed -n '/--- result.md ---/,$p'` and
  five other spellings of the same idea, in five sessions, several of them in
  sessions where `--result-only` was used minutes earlier. The default block
  put thirteen lines of coordinates in front of the answer, so the caller's
  cheapest reflex was to skip them, and skipping them also threw away the
  follow-up commands: one session then hand-assembled `envoy turn --resume
  <id>` without the `--cwd` collect had printed for it.

  The cut is not about size in aggregate — the held-back fields are ~590 bytes
  against a mean 8.9KB result. It is about where they land: the convergence
  rounds, where a reviewer answers "integrated, no new findings" in 87 bytes and
  the preamble ran seven times longer than the payload. **A field that only a
  failure makes actionable is noise on a success**, so the block is now tiered
  by status: settings, tokens, prompt-state evidence, result kind, and the log
  paths print on every non-ok turn — whose recovery prose reasons from them and
  names those three files — and on `--status-only`, which became the
  full-preamble read. Whole-output cut over 60 stored jobs: 6.4%, and 32–35% on
  the short later rounds.

  Two fields earned their exception rather than a rule. A provider-announced
  model that *disagrees* with the requested one stays in a healthy block — it is
  the only thing on that line the caller did not already know. And the bare
  `session:` line went away wherever `resume:` or `takeover:` already spells the
  id out: three copies of one identifier is three invitations to rebuild a
  follow-up by hand, which the logs show a caller doing, minus the `--cwd`.
  Checked and not changed: a fan-out cannot render a lone member header —
  `envoy fan` refuses a roster below two.

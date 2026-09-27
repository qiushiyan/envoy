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
- **2026-08-02 — the preamble callers kept `sed`-ing past.** Window `2026-07-25`
  → `2026-08-01`: of 158 `envoy collect` invocations across 40 sessions, 33
  piped the output into `head`/`tail`/`sed`, and 14 of those jumped straight to
  the payload — `sed -n '/--- result.md ---/,$p'` and five other spellings of
  one idea, some in sessions that had used `--result-only` minutes earlier.
  Thirteen lines of coordinates stood in front of every answer, so the cheapest
  reflex was to skip them — which also discarded the follow-up commands: one
  session then hand-assembled `envoy turn --resume <id>` without the `--cwd`
  collect had printed for it. The aggregate saving is small (~590 bytes against
  a mean 8.9KB result); the case is where it lands, on the convergence rounds
  where a reviewer answers in 87 bytes and the preamble ran seven times longer
  than the payload. **A field that only a failure makes actionable is noise on
  a success.**

  The first cut gated on `status != ok` and a cold review found it wrong at
  both edges: an ok turn whose `result.md` will not read is sent to `raw.log`
  by its own next line, from a block that had just withheld the paths — the
  exact "prescription pointing at files the block withheld" this change existed
  to prevent. The tier follows **delivery** (ok *and* the payload reads). The
  same round killed a carve-out that had looked principled — printing the
  settings line when the reported model differed from the requested one — since
  `opus` against `claude-opus-5` is the provider's own alias resolution, so
  inequality cannot separate resolution from substitution and the exception
  would have fired on every ordinary claude dispatch. **A gate that cannot be
  computed from an observation is inference wearing a condition.** Rebutted the
  same round: gating each log path on the recovery branch that names it. The
  three streams are one affordance, and per-branch rules buy one line on a job
  already in trouble at the price of a block whose shape you cannot predict.
- **2026-08-02 — the suite was mostly race-runtime exit sleep.** `make test`
  took ~105s and nobody had asked why. Not the code: the harness builds the CLI
  with `-race` deliberately — the runner's goroutines live in that subprocess,
  so a race-built *test* process would not see them — and the race runtime
  sleeps `atexit_sleep_ms=1000` before every process exit. Every test spawns at
  least one envoy, so the suite paid that second ~100 times. `GORACE=atexit_sleep_ms=0`
  in the harness's subprocess environment: **105s → 17s**, same tests, same race
  build, detection during the run untouched. The in-session diagnosis measured
  the symptom right (~1.1s per race-built invocation against ~0.1s plain, at 5%
  CPU) and named the cause only as "teardown"; the review named the knob.
  **A per-process cost multiplied by a serial suite is worth one measurement
  before it is worth an architecture** — splitting the target, race-building
  only the concurrency tests, and `t.Parallel()` were all considered, and all
  trade coverage or determinism for what was one env var.
- **2026-08-03 — the mid-turn codex stall is codex's, and the rollout proves
  it.** A headroom consult round 2 accepted its prompt, streamed 7 events in
  14s, then sat silent for 23 minutes with the process alive until the caller
  SIGTERMed it and finished the round by hand in interactive `codex resume` —
  raising the question of whether envoy's stdin/pipe mechanics had wedged the
  turn. The arbiter is codex's own rollout file, which writes independently of
  envoy's pipes: it shows the delivered prompt, the same last reasoning item,
  one lone `token_count` 15 minutes into the silence (348 output tokens for the
  hung response; rate limits at 7%), and nothing else — the client stalled
  mid-turn between API responses, and the manual resume of the same session
  proceeded instantly, so session state was undamaged. Not a one-off: of all
  store jobs started since 2026-07-25, six codex turns (no claude turns) ended
  more than 2 minutes after their last provider output, and a second rollout
  read showed the identical signature — a reasoning item, then nothing for 50
  minutes. Codex-cli 0.144.x client/stream defect; nothing to fix in the
  engine, and the incident validates two paid-for positions: the progress line's
  `last_provider_activity` climbing against a frozen event count is what let the
  caller see the stall, and recovery's `accepted → resume` was exactly the
  action that worked. **When a codex turn goes quiet, read the rollout before
  suspecting the harness — it is the one channel envoy cannot block.**
- **2026-08-28 — 221 caller sessions: the engine held, the handoff did not.**
  An obelisk sweep of every host session that ran envoy (1,970 invocations;
  926 jobs on disk, 96% ok; codex median 6.1m, p90 10.7m; first collect
  landing within 2 minutes of `endedAt` in 495 of 513 joins) found the friction
  almost entirely in the seam between the skills and the harness. **Coordinate
  fetch:** the skills said "relay the coordinate block", the harness hides a
  background task's stdout, so 159 sessions ran `sleep N; cat <task-output>`
  359 times, 36% of them empty and retried with longer sleeps — the single
  largest round-loser. Fixed by `--coordinate-file`; a codex consult rejected
  `jobs --newest` for lacking dispatch correlation (same-project concurrent
  dispatch, name-ordered listing, block printed before the first meta write).
  **Warm-voice refusal:** review's default `fan --with-from <consult-out-dir>`
  is refused when the consult was itself a fan-out (3 refusals, 20 sessions
  that found `…/codex` themselves) and the refusal offered the whole-set
  round, which seats no cold voice. Now flag-specific: `--with-from` gets
  copy-ready member flags through the one eligibility check. **Two causes,
  one envelope:** of 9 timeouts, 4 were the laptop leaving its network (the
  provider still streaming until 1–17m before the cap, then `Reconnecting…`
  events) and 5 were genuine stalls (2–3 events, silent for the whole cap).
  The consult also found what the analysis missed: the codex driver let a
  bare `error` event overwrite the outcome, so a turn that reconnected and
  completed (exit 0, `turn.completed`, 9 KB result — 08-26 review-r3) was
  recorded `failed`. Fixed: only `turn.failed` is a verdict; reconnects are a
  tallied observation printed as `provider stream:`. **Collect bypass:** 14
  direct `result.md` reads before any collect, driven by the harness
  persisting outputs over ~30 KB — 87 of 260 fan-outs exceed that from member
  results alone, so no baseline trimming can fix it; the skill line "collect
  once, read afterwards" is the fix. **Rollout:** the installed binary was
  from 08-02 against source from 08-20, both `0.4.0` — the stale-binary
  incident's shape again; the version is now bumped with every contract
  change and the skills edited only after `make install`.
- **2026-09-12 — the surface was the friction: one verb, named jobs.** An
  obelisk pass over 119 review/consult invocations found zero CLI refusals and
  every cost in orientation and handoff: the shared mechanics doc read on 81
  of 119 invocations and re-read 73 times, and the coordinate file's *path*
  failing to survive across the caller's shell calls — 123 reads of it across
  48 sessions, 59 `ls -t` guesses at the store in 21. Real use had two dispatch
  shapes (one voice; several voices) plus collect; `steer`, `jobs`, `turn`
  versus `fan`, and six dispatch flags were surface the caller learned without
  using. A codex consult settled the replacement: `run <job>` with the name
  chosen by the caller and reserved atomically (a taken name refused, never
  suffixed), voices as `provider[:model[:effort]]` or `@<job>`, and nothing to
  read back. Two reviews of the result, both cold codex: the first found the
  records still half-old — persisted commands with no reader, schema fallbacks,
  a member label that recorded the parent's name — plus three defects pinned
  red before the fix (a generated member address landing on a sibling's
  directory, `collectedAt` stamped before the block reached the caller's
  writer, and the "identical retry" turning a continuation into a cold voice
  without its budget); the second caught the fix's own regression (pending
  rendering an abandoned job's recovery from its still-running record). The
  lessons that generalize: **a record holds facts and the reader renders the
  words**, and **an address the caller chooses needs no handoff at all.**
- **2026-09-14 — primary-turn usage.** **A measurement keeps its scope and
  missing evidence through finalization.** The steward needed primary context
  from meta alone. A captured Claude Code 2.1.270 turn repeated usage across
  content blocks; its three unique responses had placeholder output counts,
  while terminal usage settled at 338 output tokens and `modelUsage` included
  a helper model. The sanitized capture lives at
  `internal/provider/testdata/claude-2.1.270-usage.jsonl`;
  `TestClaudeUsageCapture` pins its accounting, and controlled variants cover
  subagent attribution, compaction and incomplete terminal evidence. A separate
  real run verified live-to-settled metadata and a final context of 19,646
  tokens, equal to its last primary response's input/cache sum.
  `TestClaudeUsageLiveAndCollected` pins event-driven persistence and display.
  `DESIGN.md` § Primary-turn usage owns the contract and schema-9 rationale.
- **2026-09-14 — the two-assignment consult.** A caller gave two voices two
  different jobs (a design critique on one model, a product-landscape survey
  on the other). The fan-out sent one prompt, so the agent first wrote one
  brief that assigned the tasks by name, and the survey voice did the critique
  instead; it then split into two jobs and carried the two-job ledger the
  fan-out exists to absorb ("the landscape job is still running and lands
  separately"). The fan-out's value was always one dispatch, one wait, one
  collect — never equal prompts. A codex consult (`consult-r1` in this
  project's store) settled the shape: the prompt attaches to the voice
  (`--with voice=file`, `--prompt-file` the default), the group's prompt copy
  goes rather than becoming conditional, discovery keys on records, and two
  latent defects that change exposed — a manifest write failure that warned,
  ran the members anyway, then released the directory as unstarted; pending
  skipping a roster member with no record — closed with it. Its one rejected
  proposal, a JSON seat for the `=`-in-path corner, is recorded in
  `DESIGN.md` § Fan-out: several turns, one job. **Supervision only, zero lifecycle change — the same
  admission test the fan-out itself passed.**
- **2026-09-21 — provider launchers keep account choice outside the engine.**
  Bare provider binaries inherited the dispatching environment instead of
  following headroom's account board; the primary Codex account was at 98%
  weekly usage while another had room. `ENVOY_CODEX_CMD` and
  `ENVOY_CLAUDE_CMD` now supply literal command prefixes, resolved anew for
  every turn. **A launch failure never licenses falling back to the account
  the launcher was meant to avoid.** Stub launchers pin argv/environment/stdin,
  fresh and resumed turns, fan-out, stderr-only noise, refusal without
  fallback, session locks, and timeout/cancel cleanup of stubborn descendants
  with both exec and non-exec launchers. The default spawn path retains its
  argv and environment; optional `commandPrefix` preserves schema 9 and
  `providerArgv` semantics. A live smoke test with codex-cli 0.155.1 and
  `headroom launch --vendor codex --` completed both fresh and resumed turns
  on the board's already-selected second account: each child's command
  printed that account's `CODEX_HOME`, both records captured the prefix,
  and the resumed turn recalled the first turn's marker in the same session.
  Live board observations around those turns (10:46:18–10:47:54 UTC) moved
  the second account from 3% to 4% and the primary from 98% to 99%.
  Routing and continuation are verified; the board does not isolate these
  turns' billing, so the requested primary-unchanged check is inconclusive.
- **2026-09-21 — a refused name served somebody else's review.** Mined from
  the session index (Bash calls matching `envoy run `, tool results matching
  the refusal text; 2026-09-12 → 09-21, this session excluded): 155
  dispatches in 42 sessions, 62 of them literally `consult-r1` or
  `review-r1` — the skills hand every session the same names. 13 sessions
  hit "already exists"; 3 then collected the old job under the refused name
  (70 h, 108 h and 213 h stale). In the worst one the dispatch was
  `envoy run review-r1 … 2>&1 | tail -15`: the pipe turned exit 3 into a
  "completed (exit code 0)" task, `envoy collect review-r1` printed a
  nine-day-old `status: ok` review of other code, and only the content gave
  it away (10 piped dispatches in 6 sessions carry the same exposure). Every
  refusal that *was* read was read within a minute; no recorded session
  waited out a cap on a job that never ran. **Refusing a reused name did not
  prevent the wrong-job collect it was chosen to prevent; it caused it.** A
  name now addresses its latest generation (`DESIGN.md`); 113 of 117 flat
  records in the store were collected, so the held-name refusal that remains
  should be rare. Compare next pass: sessions hitting a name refusal (was 13 of 42), any
  collect whose `collected:` stamp predates the collecting session, and any
  caller that reads a `+N` directory as a fault. Revise if held-name
  refusals recur among sessions that were not concurrent, or if a caller
  continues the wrong conversation through `@<name>` after a reuse.
  Review r1 (codex, cold) raised that last case as foundational: caller A
  collects `consult-r1`, caller B reuses the name, and A's later
  `@consult-r1/codex` continues B's conversation — delivery proves receipt,
  not that the caller is done with the name. Measured exposure over the same
  window: 11 pairs of distinct sessions used one name in one project, 2
  within 24 h, and in none was the first session still calling envoy after
  the second dispatched. The hazard was real and so far unobserved; the user
  chose to remove it rather than watch for it, and a name is now scoped to
  its caller (`DESIGN.md`). Identity comes from the harness: Claude Code
  exports `CLAUDE_CODE_SESSION_ID`; codex-cli 0.155.1 exports no per-session
  variable to its shell (checked in the binary's strings), so a Codex caller
  keeps the unscoped meaning and its holds, unless something sets
  `ENVOY_CALLER` for it. Records written before this carry no `caller` and
  read as identity-less. Compare next pass: name refusals among Claude Code
  callers should be only self-holds; a Codex caller refused by another
  session's job is the signal that Codex needs an identity of its own.
  Review r2 kept one foundational point: the fallback cannot tell a pickup
  from a caller whose identity changed. The user chose to report it rather
  than refuse it, so a fell-back name and a continuation of another
  session's conversation each carry a `note:`. Compare next pass: how often
  the note fires, and whether callers that see it check the job or read past
  it — a note nobody acts on is a candidate for removal, a wrong pickup that
  carried one and was used anyway is the case for something stronger.
- **2026-09-27 — the read-only turn that could not read.** A tabtype consult
  (`consult-r1b`, claude-fable-5-1) finished ok after 6 minutes, but its
  result opened by saying the lesson files and the setup doc its brief cited
  were unreadable: 23 tool calls refused, reads outside the cwd plus
  multi-part Bash commands. envoy passed no permission flag on a turn without
  `--allow-write`, so Claude Code ran in `default` mode, and a headless
  session has no one to approve anything. Inheriting the user's config was
  not the fix: its `defaultMode: "auto"` is ignored by `claude -p` (2.1.283
  init reports `default`, even with `--permission-mode auto` passed), and the
  user's interactive YOLO comes from a launcher flag the provider child never
  sees. Claude turns now always run with `bypassPermissions`; the user
  trusts the prompt to keep a consult design-only. 1 other claude turn of 73
  since August had a refusal. Compare next pass: a read-only claude turn
  whose `git status` changed under it is the signal that the prompt is not
  enough.

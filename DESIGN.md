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

## Storage

Jobs live in one central store, `~/.local/state/envoy/jobs/<slug>/`, never in
the project tree (the predecessor's repo-local `.sidekick/` dirs are
deliberately gone — they polluted every repo with runtime state). The slug is
`basename-hash8`: the basename for humans scanning the store, the hash of the
symlink-resolved path for uniqueness, the git root as anchor so a dispatch
from a subdirectory belongs to the project. The store is an implementation
detail by contract: dispatch prints the out-dir, collect and pending
re-derive it from cwd, and no caller constructs the path.

## Deliberately not built

- No daemon, status command, cancel service, or job listing — the caller's
  background-task layer is the live-job layer.
- No alias translation, effort aliases, model fallbacks, or provider
  auto-selection.
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

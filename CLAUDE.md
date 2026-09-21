# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

envoy runs **headless AI-session turns** (the `claude` or `codex` CLI) as supervised, caller-named jobs and returns them as durable data: `result.md` is the return value, `meta.json` the machine-readable truth, `progress.log` the live view, `raw.log`/`stderr.log` the verbatim command streams. It is deliberately judgment-free mechanism — no retries, no review loops, no provider selection. Callers (humans, agents, scripts) own all judgment; do not grow the engine past that line.

One verb dispatches: `envoy run <job> --prompt-file F --with <voice>…`. A voice is a cold session (`provider[:model[:effort]]`) or a finished job's conversation continued (`@<job>`, `@<job>/<member>`). One voice runs flat in the job directory; several run as a **fan-out** — a prompt to each (its own, attached as `<voice>=<file>`, or the job's `--prompt-file`), one process to wait on, one exit code, a member directory each. A fan-out is **a supervisor over unchanged turns, not a second engine**: each member is an ordinary turn, and the group adds only supervision and presentation. Anything that would give a fan-out its own turn semantics belongs in a caller.

It ships as a library (root package `envoy`) and a thin CLI (`cmd/envoy`). **Zero third-party dependencies is a deliberate constraint** — the Go stdlib covers this domain; keep `go.mod` empty.

`DESIGN.md` records the why — rejected alternatives and settled non-goals; `EVIDENCE.md` is the dated log of what each one cost. Read DESIGN.md before proposing a behavior change, and add to EVIDENCE.md when usage teaches something new. The `/improve-tool` skill mines real usage for the next improvements.

## Commands

```sh
go build ./...                # build everything
make test                     # unit + integration (go test ./... -count=1)
go test ./internal/...        # unit tests only (fast, no node needed)
go test ./tests/ -run TestCodexTimeout -count=1 -v   # one integration test
make install                  # binary to ~/.local/bin/envoy — run after merging: the skills use the installed binary
go vet ./... && gofmt -l .    # lint
```

Integration tests need `node`: `tests/` builds a **race-instrumented** binary and drives it against a fake provider selected by `ENVOY_FAKE_SCENARIO`, with `HOME` pointed at a temp dir so all state (locks, job dirs, transcripts) is isolated. The `ENVOY_*_MS` env knobs shrink the lifecycle to seconds — test rig, not user configuration.

## Architecture

Flow: `cmd/envoy` (flags only) → `envoy.go` facade (roster resolution, usage errors, name reservation) → `internal/runner` (lifecycle) ↔ `internal/provider` (drivers) → `internal/job` (artifacts). `internal/collect` reads what the runner wrote (collect, pending, and the continuation inspection dispatch shares); `internal/prose` words everything either of them says to the caller; `internal/fan` sits beside the runner — it starts N runs and reports them as a set, knowing nothing about the turn lifecycle.

- **`envoy.go` resolves every voice the same way.** A cold voice, a continued job, and each member of a continued fan-out all become one `voice`, pass one set of roster checks (a conversation once, one tree and one anchor, write intent only on a single voice), and become one `runner.Options`; cardinality then selects the flat or fan-out layout and nothing else. Member addresses are allocated here, unique against the names already taken.
- **`internal/prose` owns the caller-facing vocabulary** — every situation's wording (status glosses, recovery, refusals, next actions), one situation, one wording, reviewable in one file. Surfaces keep only their own layout and local diagnostics (`collect error: …`, usage errors); everyone else contributes observations (typed fields, cause strings), never situation prose.
- **`internal/provider` is the extensibility seam.** A driver owns native argv and environment additions and translates the provider's raw stream into a small set of semantic events; the runner consumes only those and **never branches on provider name**. The runner resolves the executable prefix (`DESIGN.md` § Provider launchers). Adding a provider = one driver file + registration.
- **`internal/runner` is a single-threaded state machine.** One event-loop goroutine owns all `run` state; helper goroutines only send into its channels — never mutate `run` from another goroutine; route through `callCh`. The runner is instance-clean (no package-level state), which is what lets a fan-out run N of them in one process, and it owns its own session lock: a held session refuses that turn alone.
- **Process exit and stream EOF are separate observations, by design.** A provider grandchild can outlive the CLI and hold the pipes open, so the whole tree runs in its own process group with an exit → grace → SIGKILL escalation.
- **`internal/job` owns the record schemas for writer and reader**, so the runner and collect cannot drift. **Records hold facts, never prose**: `meta.json` carries dispatch, lifecycle, recovery and usage observations; `collect` renders commands and prescriptions from those fields plus the directory it read them from. Drivers normalize usage snapshots, the runner publishes them, and `DESIGN.md` § Primary-turn usage owns their meaning. A record from another schema version is refused by name, not reinterpreted. `group.json` is a **roster of member names plus the shared cwd, cap and baseline, never member state** — a member's name is its directory, and everything else about it lives only in that member's own `meta.json`.

## The Output Contract

Stdout blocks, progress vocabulary, recovery prose, exit codes (0 ok · 1 failed · 2 infra · 3 usage · 4 timeout · 5 interrupted · 6 partial, fan-out only), and the job-dir file set are a **caller interface, consumed by an AI agent** — the prose is a prompt surface, and integration tests assert exact strings. Changing wording is a contract change, not cosmetics. `envoy -h` is the page the agent reads before driving the engine; `TestHelpIsSelfSufficient` pins what it must teach.

Three rules govern that prose, all enforced in `internal/prose`:

- **Say what happened, what it rules out, and the one action to take next** — every non-ok status carries its gloss.
- **Prescribe only what the engine observed.** Recovery follows from prompt state, the only thing the engine can prove: `accepted` → resume, never redispatch; `not_started` → one identical retry; `unknown` → absence of output is not proof of no work.
- **Hand over runnable commands, not fragments.** A continuation names the job (`--with @<dir>`) whose records carry the session and settings, with the prompt file left as a placeholder because a resumed turn needs a NEW prompt. The retry repeats the source conversation, archived `prompt.md` and CLI settings, including the spend cap, under a new name. Its environment, including the launcher, is resolved at dispatch.

Behavioral invariants the code encodes deliberately (each has a test):

- **The name is the address.** The caller chooses the job name before dispatch and collects by it, so nothing on a hidden stdout has to be read back. A name is scoped to its caller and a directory is an identity: every dispatch under a name runs in a generation of its own (`<name>`, `<name>+2`, …), and `collect`/`@<job>` resolve a name to the caller's own newest generation, else the newest of anyone's (`job.Resolve`) — so sessions sharing a checkout reuse `review-r1` without ever reading or continuing each other's jobs. The caller is the harness's exported session identity (`ENVOY_CALLER`, else `CLAUDE_CODE_SESSION_ID`), recorded as `caller`; it is observed, never derived, and a caller without one gets the unscoped meaning. No directory ever moves, so recorded and printed paths stay true. A name that fell back to another caller's job says so in a `note:` line — the engine cannot tell work picked up on purpose from a caller whose identity changed, so it reports and never refuses. A job that is running or uncollected (`collect.NameHold` is the one definition) holds its name only against a caller that shares its meaning of the name — the same caller, or either side having no identity. A directory is reserved atomically by creating it, and a refusal that ran nothing gives the name back.
- **No model substitution.** Omitted model/effort means the provider's own config governs; the engine reports `(provider default)` and records only what the provider itself announced. Observed is recordable; inferred is forbidden — which also rules out *comparing* the two: `opus` against `claude-opus-5` is the provider's own alias resolution, so a request/report mismatch cannot prove a substitution and never gates behavior.
- **The terminal envelope wins** over a timeout that fires during cleanup.
- **Session locks are never auto-reclaimed** — a dead runner can leave a live orphan provider, so recovery inspects first. One live turn per session id, including ids that only arrive mid-stream; a refusal points at the owning job's collect.
- **The timeout is a wall-clock deadline, not a monotonic timer** (laptop sleep must not stretch the cap) — a safety cap, not a stall detector. The engine never *acts* on quiet, but a capped turn's envelope *reports* the stream it observed (events, last event, how long it had been quiet), because a cap alone reads identically whether the provider worked to the deadline or went silent right after acceptance — and those two want opposite follow-ups.
- **`collectedAt` means the block reached a caller.** The block is rendered in full, written, and stamped only after the write succeeded; `--status-only` and `pending` never stamp; an ok turn whose `result.md` will not read stays owed.
- **A collected block is tiered by whether the payload landed, not by status.** A turn reporting ok whose `result.md` reads holds back the diagnostic tier (settings, tokens, context usage, prompt state and evidence, result kind, log paths) and prints only what the caller acts on; everything else prints in full, and `--status-only` is the full-preamble read. Status alone is wrong at both edges — see DESIGN.md, "Collection: delivery, not display".
- **A continuation is anchored on job records, not remembered session ids.** `@<job>` reads the session and unspecified settings from the named job's `meta.json` (explicit `--cwd`/`--baseline` win; the provider cannot change; the cap is always the new dispatch's own), and eligibility has one definition — `continuationBlocker` in `internal/collect` — consulted by collect's resume line and by dispatch, so collect never advertises a continuation dispatch would refuse (a running turn's session prints without its resume command). A continued conversation may appear once per roster; a write-recorded source continues alone, since fan-out members are read-only and write intent may only widen; every continued turn records `resumedFrom`.
- **One member's outcome licenses nothing about another.** `6 partial` says results landed *and* a member needs a decision; recovery is per member — no group-wide retry — and `--allow-write` is refused on a fan-out because members share one tree. A round is not recovery: `@<fan-out>` alone continues every member on one NEW prompt as a new fan-out, whole at eligibility (a member without a session refuses the round) while a session held at dispatch time refuses that member alone.
- **A transient provider error never outranks its verdict.** codex's bare `error` events (reconnects, dropped streams) are observations tallied into `connectionErrors`; only `turn.failed` fails a turn, and an observed `turn.completed` with a result is ok whatever came before it. The tally prints as `provider stream: …` in the diagnostic tier, worded as what the provider reported — never "offline" — and nothing acts on it.

## Provider CLI Drift

Driver argv baselines are Claude Code 2.1.207 and codex-cli 0.144.1; Claude usage parsing also has captured 2.1.270 coverage (`EVIDENCE.md`). After a provider CLI upgrade, re-check `--help` and the stream shapes before blaming a parser.

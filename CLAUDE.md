# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

envoy runs **one headless AI-session turn** (the `claude` or `codex` CLI) as a supervised job and returns it as durable data: `result.md` is the return value, `meta.json` the machine-readable truth, `progress.log` the live view, `raw.log`/`stderr.log` the verbatim provider streams. It is deliberately judgment-free mechanism — no retries, no review loops, no provider selection. Callers (humans, agents, scripts) own all judgment; do not grow the engine past that line.

`envoy fan` runs one prompt on several turns as a single job, and `fan --resume-from` continues a finished fan-out as a new round. Either way it is **a supervisor over unchanged turns, not a second engine**: each member is an ordinary turn in its own subdirectory, and the group adds only supervision and presentation. Anything that would give a fan-out its own turn semantics belongs in a caller.

`envoy steer` routes a supplemental prompt ("forgot to mention X") for a dispatched job. It never delivers into a live turn — no provider accepts input into one: claude's streaming input queues it as a separate turn, codex exec reads stdin once at dispatch (both verified live, see EVIDENCE.md 2026-07-28) — so its whole contract is the honest report: "not delivered", why, and the runnable follow-up command with the supplement file already filled into the prompt slot. It reads without mutating anything.

It ships as a library (root package `envoy`) and a thin CLI (`cmd/envoy`). **Zero third-party dependencies is a deliberate constraint** — the Go stdlib covers this domain; keep `go.mod` empty.

[DESIGN.md](DESIGN.md) records the why — rejected alternatives and settled non-goals; [EVIDENCE.md](EVIDENCE.md) is the dated log of what each one cost. Read DESIGN.md before proposing a behavior change, and add to EVIDENCE.md when usage teaches something new. The `/usage-lab` skill mines real usage for the next improvements.

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

Flow: `cmd/envoy` (flags only) → `envoy.go` facade (validation, usage errors) → `internal/runner` (lifecycle) ↔ `internal/provider` (drivers) → `internal/job` (artifacts). `internal/collect` reads what the runner wrote (collect, jobs, pending, steer); `internal/prose` words everything either of them says to the caller; `internal/fan` sits beside the runner — it starts N runs and reports them as a set, knowing nothing about the turn lifecycle.

- **`internal/prose` owns the caller-facing vocabulary** — every situation's wording (status glosses, recovery, refusals, next actions), one situation, one wording, reviewable in one file. Surfaces keep only their own layout and local diagnostics (`collect error: …`, usage errors); everyone else contributes observations (typed fields, cause strings), never situation prose. (The package was `internal/steer` until the steer command took the name.)
- **`internal/provider` is the extensibility seam.** A driver owns argv/env and translates the provider's raw stream into a small set of semantic events; the runner consumes only those and **never branches on provider name**. Adding a provider = one driver file + registration.
- **`internal/runner` is a single-threaded state machine.** One event-loop goroutine owns all `run` state; helper goroutines only send into its channels — never mutate `run` from another goroutine; route through `callCh`. The runner is also instance-clean (no package-level state), which is what lets a fan-out run N of them in one process.
- **Process exit and stream EOF are separate observations, by design.** A provider grandchild can outlive the CLI and hold the pipes open, so the whole tree runs in its own process group with an exit → grace → SIGKILL escalation.
- **`internal/job` owns the meta.json schema for both writer and reader**, so the runner and collect cannot drift. Readers distinguish an absent field from an explicit `null` (`collectedAt: null` means "not yet collected"). `group.json` is a **roster of coordinates, never member state** — a member's status lives only in that member's own `meta.json`.

## The Output Contract

Stdout blocks, progress vocabulary, recovery prose, exit codes (0 ok · 1 failed · 2 infra · 3 usage · 4 timeout · 5 interrupted · 6 partial, fan-out only), and the job-dir file set are a **caller interface, frequently consumed by an AI agent** — the prose is a prompt surface, and integration tests assert exact strings. Changing wording is a contract change, not cosmetics.

Three rules govern that prose, all enforced in `internal/prose`:

- **Say what happened, what it rules out, and the one action to take next** — every non-ok status carries its gloss.
- **Prescribe only what the engine observed.** Recovery follows from prompt state, the only thing the engine can prove: `accepted` → resume, never redispatch; `not_started` → one identical retry; `unknown` → absence of output is not proof of no work.
- **Hand over runnable commands, not fragments**, carrying the settings the turn was dispatched with — with the prompt file left as a placeholder, because a resumed turn needs a NEW prompt. The one closer of that slot is `envoy steer`, whose supplement file already exists and is filled in. The engine names no particular caller harness.

Behavioral invariants the code encodes deliberately (each has a test):

- **No model substitution.** Omitted `--model`/`--effort` means the provider's own config governs; the engine reports `(provider default)` and records only what the provider itself announced. Observed is recordable; inferred is forbidden.
- **The terminal envelope wins** over a timeout that fires during cleanup.
- **Session locks are never auto-reclaimed** — a dead runner can leave a live orphan provider, so recovery inspects first. One live turn per session id, including ids that only arrive mid-stream.
- **The timeout is a wall-clock deadline, not a monotonic timer** (laptop sleep must not stretch the cap) — a safety cap, not a stall detector. The engine never *acts* on quiet, but a capped turn's envelope *reports* the stream it observed (events, last event, how long it had been quiet), because a cap alone reads identically whether the provider worked to the deadline or went silent right after acceptance — and those two want opposite follow-ups.
- **`collectedAt` means the result reached a caller.** `--status-only`, `jobs`, and `pending` never stamp; an ok turn whose `result.md` will not read stays owed.
- **A collected block is tiered by status.** A turn that returned a result prints only what the caller acts on — coordinate, status, duration, follow-up commands, payload, next action. The diagnostic tier (provider/model/effort/label, tokens, prompt state and its evidence, result kind, the three log paths, the bare `session:` line) prints on every non-ok turn, whose recovery prose reasons from those fields and names those files — and on `--status-only`, which is the full-preamble read. Two exceptions survive into a healthy block: a provider-announced model that disagrees with the requested one, and the session id when no command below already carries it.
- **Steer answers, never delivers.** Every steer report opens "not delivered"; a non-ok terminal job is routed to collect, not handed a resume its recovery has not licensed; a fan-out gets per-member steer lines. Steer stamps nothing, reconciles nothing, and rewrites nothing.
- **A continuation is anchored on job records, not remembered session ids.** `turn --resume-from <job-dir>` and `fan --with-from <job-dir>` read the session and unspecified settings from the named job's meta (explicit flags win; the provider cannot change; the cap is always the new dispatch's own), and eligibility has one definition — `memberResumeBlocker` — consulted by collect's resume line and both dispatch inspections, so collect never advertises a continuation dispatch would refuse (a running turn's session prints without its resume command). Blockers refuse before anything spawns; a continued conversation may appear once per roster; a write-recorded source is refused on a fan-out (members are read-only — write intent may only widen, on `turn --resume-from`); a warm source's cwd and baseline inherit and must agree across sources; every continued turn records `resumedFrom` lineage.
- **One member's outcome licenses nothing about another.** `6 partial` says results landed *and* a member needs a decision; recovery is per member — no group-wide retry — and `--allow-write` is refused on a fan-out because members share one tree. A round is not recovery: `fan --resume-from` continues every member on one NEW prompt as a new fan-out, whole or refused, reserving every member's session before any turn spawns.

## Provider CLI Drift

The drivers encode facts verified against Claude Code 2.1.207 and codex-cli 0.144.1. After a provider CLI upgrade, re-check `--help` and the stream shapes before blaming a parser.

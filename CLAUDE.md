# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

envoy runs **one headless AI-session turn** (the `claude` or `codex` CLI) as a supervised job and returns it as durable data: `result.md` is the return value, `meta.json` the machine-readable lifecycle/recovery truth, `progress.log` the live view, `raw.log`/`stderr.log` the verbatim provider streams. It is deliberately judgment-free mechanism — no retries, no review loops, no provider auto-selection. Callers (humans, agents, or scripts) own all judgment; do not grow the engine past that line.

It ships as a library (root package `envoy` — `Turn`, `Collect`, `Pending`) and a thin CLI (`cmd/envoy`). **Zero third-party dependencies is a deliberate constraint** — the Go stdlib covers this domain; keep `go.mod` empty.

[DESIGN.md](DESIGN.md) records the why — rejected alternatives, settled non-goals, and the evidence log of paid-for lessons. Read it before proposing a behavior change; update its log when an incident teaches something new.

## Commands

```sh
go build ./...                # build everything
make test                     # unit + integration (go test ./... -count=1)
go test ./internal/...        # unit tests only (fast, no node needed)
go test ./tests/ -run TestCodexTimeout -count=1 -v   # one integration test
make install                  # build binary to ~/.local/bin/envoy
go vet ./... && gofmt -l .    # lint
```

Integration tests need `node` on PATH: `tests/TestMain` builds a **race-instrumented** binary and runs it against `tests/fake-provider.mjs` (via the `claude`/`codex` shims in `tests/fake-bin/`), selected by `ENVOY_FAKE_SCENARIO`. Tests isolate all state by pointing `HOME` at a temp dir — locks, default job dirs, and claude transcript reads all resolve under `$HOME`. The `ENVOY_*_MS` env knobs (heartbeat, timeout poll, SIGKILL escalation, close grace) exist so the full lifecycle runs in seconds; they are test rig, not user configuration.

## Architecture

Flow: `cmd/envoy` (flags only) → `envoy.go` facade (validation, usage errors) → `internal/runner` (lifecycle) ↔ `internal/provider` (drivers) → `internal/job` (artifacts). `internal/collect` reads what the runner wrote.

- **`internal/provider` is the extensibility seam.** A `Driver` is stateful per turn: it owns argv/env construction, accumulates the provider's stream, and translates raw JSON lines into five semantic event kinds (`Activity`, `SessionStarted`, `Accepted`, `Terminal`, `Note`). The runner consumes only those events and **never branches on provider name**. Adding a provider = one driver file + a case in `provider.New` + an entry in the `efforts` map.
- **`internal/runner` is a single-threaded state machine.** One event-loop goroutine owns all `run` state, unsynchronized on purpose; helper goroutines (stream pumps, process wait, timers via `afterFunc`) only send into its channels. Never mutate `run` fields from a new goroutine — route through `callCh`.
- **Process exit and stream EOF are separate observations, by design.** `spawn` passes raw `os.Pipe` fds (not exec's managed pipes) so `wait()` sees pure process exit while pumps see EOF only when every pipe writer is gone. The exit → close-grace → residual-cleanup → SIGKILL escalation in `terminate.go` exists because a provider grandchild can outlive the CLI and hold the pipes open; the whole tree runs in its own process group so it can be stopped.
- **`internal/job` owns the meta.json schema for both writer and reader.** The runner and collect share `job.Meta`, so they cannot drift. `collect` distinguishes an absent field from an explicit `null` (see `ReadMetaFile`'s raw map — `collectedAt: null` means "not yet collected", missing means legacy).

## The Output Contract

Stdout blocks, progress vocabulary, recovery prose, exit codes (0 ok · 1 failed · 2 infra · 3 usage · 4 timeout · 5 interrupted), and the job-dir file set are a **caller interface, frequently consumed by an AI agent** — the prose is a prompt surface, and integration tests assert exact strings. Changing wording is a contract change, not cosmetics; update tests and think about the agent reading it.

Behavioral invariants the code encodes deliberately (each has a test):

- **No model substitution.** Omitted `--model`/`--effort` means the provider's own config governs; the engine reports `(provider default)`, never an inferred value. When the provider announces its resolved model (claude's `system/init`), that lands as the `providerReportedModel` observation — the line between *observed* and *inferred* is the rule; alias translation inside the engine stays forbidden. Effort values are validated pre-spawn (claude silently degrades on bad effort; codex burns a turn on a 400).
- **The terminal envelope wins.** An observed `turn.completed`/`result` beats a timeout that fires during cleanup.
- **Session locks are never auto-reclaimed** — a dead runner can leave a live orphan provider; recovery must inspect first. One live turn per session id; a fresh codex thread id arrives mid-stream and a collision stops the turn rather than continuing unlocked.
- **The timeout is a wall-clock deadline comparison, not a monotonic timer** (laptop sleep must not stretch the cap), and it is a safety cap, not a stall detector.
- **Prompt-state recovery**: `accepted` → resume, never redispatch; `not_started` → one identical retry; `unknown` → absence of output is not proof of no work.
- **Claude's result envelope session id overrides the preflight id** (a resumed conversation continues under a fresh id).

## Provider CLI Drift

The argv builders and event parsing in `claude.go`/`codex.go` encode facts verified against Claude Code 2.1.207 and codex-cli 0.144.1. After a provider CLI upgrade, re-check `--help` and the stream shapes before blaming a parser. The predecessor reference implementation (same contract, JavaScript) lives at `~/dotfiles/claude/.claude/skills/sidekick-runtime/`.

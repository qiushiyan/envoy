# envoy

Run one headless AI-session turn (`claude` or `codex`) as a background-friendly
job and get it back as durable data. envoy is the engine behind the `/consult`,
`/review`, and `/delegate` Claude Code skills: the skills own judgment, envoy
owns mechanism — one turn in, files out, zero retries, zero gates.

It is a Go rewrite (and superset) of the earlier `sidekick-runtime` Node
scripts, with the same caller contract. Zero third-party dependencies.

## Install

```sh
go install ./cmd/envoy        # into $GOBIN (or GOPATH/bin)
# or
make install                  # builds to ~/.local/bin/envoy
```

## Usage

```sh
envoy turn --provider codex --prompt-file brief.md --timeout-min 30 --label consult
envoy collect <out-dir>       # print + stamp one job (default: latest for this repo)
envoy pending [--base DIR]    # discovery-only recovery index after a missed notification
envoy version
```

`envoy turn` prints a startup coordinate block (out-dir, watch command,
session, takeover), runs the provider to terminal state, then prints a
terminal block. Its stdout is written for the agent driving it: every status
carries what it rules out, and every failure ends in one runnable next
command. The durable files in the job dir are authoritative:

| File | Meaning |
|---|---|
| `prompt.md` | the exact dispatched prompt |
| `result.md` | final provider text, or failure + recovered partial output |
| `meta.json` | machine-readable lifecycle/recovery truth (atomically replaced) |
| `progress.log` | runner-owned semantic progress + 30s heartbeat |
| `raw.log` | verbatim provider stdout (stream-json / JSONL) |
| `stderr.log` | provider stderr |
| `last-message.txt` | codex's `-o` recovery surface (codex turns only) |

All jobs live in one central store — nothing is ever written inside the
project tree: `~/.local/state/envoy/jobs/<project-slug>/<stamp>-<label>/`,
where the slug identifies the project (git root when in a repo, cwd
otherwise). Callers never construct that path: dispatch prints the `out-dir:`
coordinate, and `envoy collect` / `envoy pending` re-derive the project's
store from the current directory alone. Session locks live in
`~/.local/state/envoy/locks/` — one live turn per session, never auto-reclaimed.

Exit codes: `0` ok · `1` provider failure · `2` infra · `3` usage · `4` timeout
· `5` interrupted.

## Design invariants (inherited from sidekick-runtime)

- **A screen is not an API.** Providers run headless; results are parsed from
  their JSON event streams, never scraped.
- **No model substitution.** Omitted `--model`/`--effort` means the provider's
  own config governs; the runner reports `(provider default)`, never a guess.
  When the provider itself announces the model it resolved (claude's init
  event), that is recorded as the `providerReportedModel` observation and
  shown by collect — observed, never inferred.
- **The timeout is a hard wall-clock safety cap**, compared against a fixed
  deadline (laptop sleep cannot stretch it). It is not a stall detector.
- **The terminal envelope wins.** If the cap fires while an already-complete
  turn is draining, the observed success is published, not a timeout.
- **Prompt-state recovery.** `accepted` → continue the session with a new
  prompt, never re-send the original; `not_started` → one identical retry is
  safe; `unknown` → absence of output is not proof of no work. The recovery
  line hands over the complete follow-up command, carrying the settings the
  turn was dispatched with.
- **Process-group lifecycle.** Providers run in their own group; stop
  escalates SIGTERM → SIGKILL and survives grandchildren holding the pipes.

## Library

The root package is an embeddable facade over the same engine:

```go
// TimeoutMin's zero value is the 30-minute safety cap, not "uncapped";
// running without a cap takes an explicit NoTimeout: true.
envoy.Turn(envoy.TurnRequest{Provider: "codex", PromptFile: "brief.md"})
envoy.Collect(outDir, os.Stdout, os.Stderr)
envoy.Pending(base, os.Stdout, os.Stderr)
```

Providers implement `internal/provider.Driver`; adding one is a single driver
file, the lifecycle never changes. Everything envoy says to its caller is
worded in `internal/steer` — one situation, one wording.

## Development

```sh
make test     # unit + integration (integration runs a race-instrumented binary)
make vet
```

Integration tests exercise the built binary against `tests/fake-provider.mjs`
(needs `node` on PATH) — streaming, fragmented UTF-8, timeouts, interrupts,
stubborn grandchildren, lock collisions, transcript recovery — without billing
a model. Runtime tunables (`ENVOY_HEARTBEAT_MS`, `ENVOY_TIMEOUT_POLL_MS`,
`ENVOY_SIGKILL_AFTER_MS`, `ENVOY_CLOSE_GRACE_MS`) exist for the tests.

Argv facts verified against Claude Code 2.1.207 and codex-cli 0.144.1 — after
a provider CLI upgrade, re-check `--help` before blaming a parser.

# envoy

Run one headless AI-session turn (`claude` or `codex`) as a background-friendly
job and get it back as durable data.

## Install

```sh
go install ./cmd/envoy        # into $GOBIN (or GOPATH/bin)
# or
make install                  # builds to ~/.local/bin/envoy
```

## Usage

```sh
envoy turn --provider codex --prompt-file brief.md --timeout-min 30 --label consult
envoy turn --resume-from <job-dir> --prompt-file next.md   # continue a finished job's session
envoy fan --prompt-file brief.md --with codex --with claude:opus   # one prompt, N models
envoy fan --prompt-file review.md --with-from <job-dir> --with claude:opus
                              # a continued conversation beside a cold voice
envoy collect <out-dir>       # print + stamp one job (default: latest for this repo)
envoy steer --prompt-file more.md [out-dir]   # route a supplement to a dispatched job
envoy jobs [--all] [--base DIR]   # this project's jobs, newest first, with the dir each takes
envoy pending [--base DIR]    # discovery-only recovery index after a missed notification
envoy version
```

`envoy turn` prints a startup coordinate block (out-dir, watch command,
session, takeover), runs the provider to terminal state, then prints a
terminal block. Its stdout is written for the agent driving it: every status
carries what it rules out, and every failure ends in one runnable next
command. The durable files in the job dir are authoritative:

| File               | Meaning                                                         |
| ------------------ | --------------------------------------------------------------- |
| `prompt.md`        | the exact dispatched prompt                                     |
| `result.md`        | final provider text, or failure + recovered partial output      |
| `meta.json`        | machine-readable lifecycle/recovery truth (atomically replaced) |
| `progress.log`     | runner-owned semantic progress + 30s heartbeat                  |
| `raw.log`          | verbatim provider stdout (stream-json / JSONL)                  |
| `stderr.log`       | provider stderr                                                 |
| `last-message.txt` | codex's `-o` recovery surface (codex turns only)                |

`envoy fan` sends one prompt to several turns at once and supervises them as a
single job — one background command, one completion, one collect. A member is
`provider[:model[:effort]]`, and each one is an ordinary turn with its own
session and job dir in a subdirectory named after it, so recovery stays per
member. The fan-out dir adds `group.json` (the member roster and shared
settings) and the shared `prompt.md`; `envoy collect <fan-out-dir>` prints every
member's status and result in one block, split by member name. Fan-outs are
read-only: `--allow-write` is refused because members share one working tree.

A finished job's session is a reusable asset: `envoy turn --resume-from
<job-dir>` dispatches a follow-up turn into that job's conversation, reading
the session and settings from the job's own records (explicit flags override;
the provider cannot change). `envoy fan --with-from <job-dir>` does the same
for one member of a new fan-out, beside fresh `--with` members — a voice that
already holds the context of an earlier phase (a consult, say) next to a cold
one that judges without it. The records are the safer anchor than a
remembered session id: a resumed claude conversation continues under a fresh
id, and only the job's `meta.json` names the current one. Every continued
turn records its `resumedFrom` lineage and prints it at dispatch and collect.

`envoy steer` answers the "forgot to mention X" moment after a dispatch. No
provider accepts input into a running turn — claude's streaming input would
queue the supplement as a separate turn, codex reads its instructions once at
dispatch — so nothing is ever injected: steer inspects the job and replies
"not delivered" plus the one command that does carry the supplement, the
follow-up turn continuing the same session with your file already in its
`--prompt-file` slot. On a fan-out it hands each member its own steer line. It
reads without mutating and never marks anything collected.

All jobs live in one central store — nothing is ever written inside the
project tree: `~/.local/state/envoy/jobs/<project-slug>/<stamp>-<label>/`,
where the slug identifies the project (git root when in a repo, cwd
otherwise). Callers never construct that path: dispatch prints the `out-dir:`
coordinate, and `envoy collect` / `envoy jobs` / `envoy pending` re-derive the
project's store from the current directory alone — `envoy jobs` lists the
coordinates themselves when a caller no longer has one. Session locks live in
`~/.local/state/envoy/locks/` — one live turn per session, never auto-reclaimed.

Exit codes: `0` ok · `1` provider failure · `2` infra · `3` usage · `4` timeout
· `5` interrupted · `6` partial (fan-out only: some members returned a result
and others did not).

## Design invariants

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
- **A fan-out supervises unchanged turns.** Members keep every single-turn
  invariant; the group adds only supervision and presentation. `group.json` is
  a roster of coordinates, never a copy of member state, and one member's
  outcome licenses nothing about another.
- **Steer answers, never delivers.** No provider accepts input into a live
  turn, so `envoy steer` hands over the follow-up command that carries the
  supplement instead of pretending to inject it (EVIDENCE.md, 2026-07-28, has
  the verified provider research behind this).

## Library

The root package is an embeddable facade over the same engine:

```go
// TimeoutMin's zero value is the 30-minute safety cap, not "uncapped";
// running without a cap takes an explicit NoTimeout: true.
envoy.Turn(envoy.TurnRequest{Provider: "codex", PromptFile: "brief.md"})
envoy.Fan(envoy.FanRequest{With: []string{"codex", "claude:opus"}, PromptFile: "brief.md"})
envoy.Collect(outDir, os.Stdout, os.Stderr)
envoy.Steer(envoy.SteerRequest{PromptFile: "more.md"})
envoy.Pending(base, os.Stdout, os.Stderr)
```

Providers implement `internal/provider.Driver`; adding one is a single driver
file, the lifecycle never changes. Everything envoy says to its caller is
worded in `internal/prose` — one situation, one wording.

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

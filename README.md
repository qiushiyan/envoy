# envoy

Run headless AI-session turns (`claude` or `codex`) as background-friendly,
caller-named jobs and get them back as durable data.

## Install

```sh
go install ./cmd/envoy        # into $GOBIN (or GOPATH/bin)
# or
make install                  # builds to ~/.local/bin/envoy
```

## Usage

```sh
envoy run review-r1 --prompt-file brief.md --with codex --timeout-min 30
envoy run consult-r1 --prompt-file brief.md --with codex --with claude:opus   # one prompt, N voices
envoy run consult-r1 --with codex=survey.md --with claude:opus=critique.md    # a prompt per voice, still one job
envoy run review-r2 --prompt-file next.md --with @review-r1                  # continue a finished job's conversation
envoy run review-r1 --prompt-file brief.md --with @consult-r1/codex --with codex
                              # a continued conversation beside a cold voice
envoy collect review-r1       # print one job and mark it collected
envoy collect --status-only review-r1   # the full preamble, no result body, no stamp
envoy pending [--base DIR]    # jobs still needing attention after a missed completion
envoy version
```

The caller names the job, so nothing printed by a dispatch has to be read
back: run it in the background, let the process exit, then collect by the same
name. A name means the latest job *your session* dispatched under it — each
dispatch runs in its own directory beside the earlier ones — so sessions
sharing a checkout reuse the same names without reading each other's jobs. A
voice
is `provider[:model[:effort]]` for a cold session; omitting the model or
effort hands the choice to the provider's own configuration, and the engine
never substitutes one of its own. `@<job>` continues
a finished job's conversation with the session and settings read from its
records, and `@<job>/<member>` names one member of a fan-out.

Stdout is written for the agent driving it: every status carries what it
rules out, and every failure ends in one runnable next command. `envoy collect`
follows the same economy — a turn that reports ok and whose result reads
prints its payload and the command that continues it, while a turn you now
have to investigate also prints the settings, counts, prompt-state evidence,
and log paths that diagnosis needs (`--status-only` prints that preamble on
demand). The durable files in the job dir are authoritative:

- **`prompt.md`:** the exact dispatched prompt.
- **`result.md`:** final provider text, or failure + recovered partial output.
- **`meta.json`:** machine-readable lifecycle and recovery facts, atomically
  replaced, plus supported usage measurements; collect renders commands from it.
- **`progress.log`:** runner-owned semantic progress + 30s heartbeat.
- **`raw.log`:** verbatim stdout, including provider events and launcher output.
- **`stderr.log`:** provider and launcher stderr.
- **`last-message.txt`:** codex's `-o` recovery surface (codex turns only).

Claude jobs expose live primary-turn context samples in `meta.json.usage`;
`collect --status-only` shows the context diagnostic, with unknown or incomplete
evidence explicit. The contract lives in `DESIGN.md` § Primary-turn usage.

Several `--with` voices run as a fan-out: a prompt to each — the file attached
to the voice (`--with codex=survey.md`) or the job's `--prompt-file` for any
voice without one — supervised as a single job: one background command, one
completion, one collect. Each member is an ordinary turn with its own session,
prompt and job dir in a subdirectory named after it (`codex`, `claude-opus`, a
repeat numbered), so recovery stays per member. The fan-out dir adds only
`group.json` (the roster and shared settings); `envoy collect <job>` prints
every member's status and result in one block, split by member name. Fan-outs
are read-only: `--allow-write` is refused because members share one working
tree. `--with @<fan-out>` alone continues every member as a new round, and
`--with @<fan-out>=next.md` is the same round with the prompt attached.

A finished job's session is a reusable asset. The records are the safer anchor
than a remembered session id: a resumed claude conversation continues under a
fresh id, and only the job's `meta.json` names the current one. Explicit
`--cwd` and `--baseline` override the recorded ones; the provider cannot
change; the cap is always the new dispatch's own. Every continued turn records
its `resumedFrom` lineage and prints it at dispatch and collect. A supplement
for a dispatched job ("forgot to mention X") is a follow-up turn continuing
it: no provider accepts input into a live turn.

All jobs live in one central store — nothing is ever written inside the
project tree: `~/.local/state/envoy/jobs/<project-slug>/<name>/`, where the
slug identifies the project (git root when in a repo, cwd otherwise) from the
directory the command runs in, for `run` and `collect` alike. A job argument
starting with `/`, `.` or `~` is a directory instead. Session locks live in
`~/.local/state/envoy/locks/` — one live turn per session, never auto-reclaimed.

Exit codes of a run: `0` ok · `1` provider failure · `2` infra · `3` usage ·
`4` timeout · `5` interrupted · `6` partial (fan-out only: some members
returned a result and others did not). A fan-out with no result exits with its
worst member's code; collect exits `0` whenever it printed the job.

## Running providers through a launcher

Set `ENVOY_CODEX_CMD` or `ENVOY_CLAUDE_CMD` to a command prefix. For example,
`headroom` is one possible launcher that selects an account when each turn starts:

```sh
export ENVOY_CODEX_CMD="headroom launch --vendor codex --"
export ENVOY_CLAUDE_CMD="headroom launch --"
```

Unset, empty or whitespace-only values use the bare `codex` / `claude` from
PATH. Otherwise envoy splits on whitespace and appends the provider's normal
arguments. No shell runs: quotes, escapes, variables and wildcards are literal;
paths or arguments containing spaces are unsupported in the prefix. Use a
wrapper executable on PATH for more complex setup.

Every dispatch, including a continuation, retry or fan-out member, reads its
provider's current environment setting; the recorded prefix does not pin an
account. Missing or refusing launchers yield `infra` (exit 2), with no fallback.
Their stderr lands in `stderr.log`; stderr alone does not fail a turn. Dispatch
and the collect diagnostic tier show the configured prefix as `launcher:`;
failure guidance names its environment variable before any follow-up.

`meta.json.commandPrefix` records the resolved executable and prefix arguments,
including `["codex"]` / `["claude"]` for defaults. The executed argv is
`commandPrefix + providerArgv[1:]`; `providerArgv` retains its provider-native
meaning. This optional observation retains schema 9: an absent field in an
older record means unavailable, and other schema versions remain refused.

## Design invariants

- **A screen is not an API.** Providers run headless; results are parsed from
  their JSON event streams, never scraped.
- **No model substitution.** Omitted model/effort means the provider's own
  config governs; the runner reports `(provider default)`, never a guess. When
  the provider itself announces the model it resolved (claude's init event),
  that is recorded as the `providerReportedModel` observation — observed,
  never inferred. The request and the observation are never compared to
  conclude anything: `opus` resolving to `claude-opus-5` is the provider's own
  aliasing, not a substitution to report.
- **The timeout is a hard wall-clock safety cap**, compared against a fixed
  deadline (laptop sleep cannot stretch it). It is not a stall detector.
- **The terminal envelope wins.** If the cap fires while an already-complete
  turn is draining, the observed success is published, not a timeout.
- **Prompt-state recovery.** `accepted` → continue the session with a new
  prompt, never re-send the original; `not_started` → one identical retry is
  safe; `unknown` → absence of output is not proof of no work. The recovery
  line hands over the complete follow-up command, rendered from the job's
  records at collect time.
- **Process-group lifecycle.** Providers run in their own group; stop
  escalates SIGTERM → SIGKILL and survives grandchildren holding the pipes.
- **A fan-out supervises unchanged turns.** Members keep every single-turn
  invariant; the group adds only supervision and presentation. `group.json` is
  a roster, never a copy of member state, and one member's outcome licenses
  nothing about another.

## Library

The root package is an embeddable facade over the same engine:

```go
// TimeoutMin's zero value is the 30-minute safety cap, not "uncapped";
// running without a cap takes an explicit NoTimeout: true.
envoy.Run(envoy.RunRequest{Job: "review-r1", With: []string{"codex"}, PromptFile: "brief.md"})
envoy.Run(envoy.RunRequest{Job: "consult-r1", With: []string{"codex", "claude:opus"}, PromptFile: "brief.md"})
envoy.Run(envoy.RunRequest{Job: "consult-r1", With: []string{"codex=survey.md", "claude:opus=critique.md"}})
envoy.Collect(envoy.CollectRequest{Job: "review-r1"})
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

Integration tests exercise the built binary against `tests/fake-bin/` (needs
`node` on PATH) — streaming, fragmented UTF-8, timeouts, interrupts, stubborn
grandchildren, lock collisions, transcript recovery — without billing a model.
Runtime tunables (`ENVOY_HEARTBEAT_MS`, `ENVOY_TIMEOUT_POLL_MS`,
`ENVOY_SIGKILL_AFTER_MS`, `ENVOY_CLOSE_GRACE_MS`) exist for the tests.

Provider verification baselines live in `CLAUDE.md` and the captured evidence
in `EVIDENCE.md`. After a CLI upgrade, re-check `--help` and stream shapes
before blaming a parser.

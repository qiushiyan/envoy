# Providers

A provider is the CLI a turn runs: `claude` or `codex`. This doc is why the
provider seam is shaped the way it is — what a driver owns, how the engine
treats models, effort and permissions, why a transient error never fails a
turn, and how launchers keep account choice outside the engine.

## The driver seam

A driver (`internal/provider`) owns provider-native argv and environment
additions and translates the provider's raw stream into a small set of
semantic events; the runner consumes only those and never branches on
provider name. Adding a provider is one driver file plus one entry in the
provider registry, which every surface naming providers reads — voice
parsing, effort validation, the spend-cap check, the help page.

## Models and effort: observed, never inferred

An omitted model or effort hands the choice to the provider's own
configuration; the engine reports `(provider default)` and records only what
the provider itself announced — claude's init-event model lands in
`providerReportedModel`. Alias translation (`opus` → some concrete id) is
rejected permanently: the mapping is the provider's, it changes under the
engine's feet, and the first disagreement would be a silent model
substitution. The request and the observation are never compared to conclude
anything either: `opus` resolving to `claude-opus-5` is the provider's own
aliasing, so a mismatch cannot prove a substitution and never gates behavior.

Effort takes native provider vocabulary only, validated before spawn, because
the providers fail differently on a bad value: claude silently degrades, and
codex burns a turn on an API 400.

## Permissions

envoy restricts no session: `--allow-write` records intent and anchors the
diff, and "analyse only" is a prompt convention.

- **claude** runs every turn with the fixed `--permission-mode
  bypassPermissions`. Left to inherit, a headless turn resolves its mode from
  the project's settings, the model and the `auto` classifier, and where that
  lands on `default` there is no one to answer a prompt: reads outside the cwd
  are refused, and a consult answered without the files its brief cited
  (`EVIDENCE.md`, 2026-09-27). A mode keyed to `--allow-write` would be a
  read-only sandbox by another name.
- **codex** runs under its own configuration. A derived read-only sandbox once
  broke the calling session's own tooling, so the engine passes no sandbox
  flag.

There are no allow-lists and no `--add-dir`. The signal to revisit this is a
read-only claude turn whose `git status` changed under it: the prompt was not
enough.

## A transient error is an observation; only the verdict fails a turn

codex emits a bare `error` event per reconnect attempt and then carries on; a
driver that let the last one stand as the outcome recorded `failed` over a
completed turn with a full result (`EVIDENCE.md`, 2026-08-28). `turn.failed`
is the verdict, and an observed `turn.completed` with a result is ok whatever
came before it. The reconnect events are tallied (`connectionErrors` in
`meta.json`, `provider stream:` in collect's diagnostic tier) and worded as
what the provider reported — never "offline", which the engine cannot know —
and nothing acts on the tally: the cap does not pause, and recovery still
follows prompt state alone.

## Launchers

`ENVOY_CODEX_CMD` and `ENVOY_CLAUDE_CMD` supply a command prefix (`headroom
launch --vendor codex --`, say) that the runner prepends to the driver's argv;
`README.md` § Running providers through a launcher and `envoy -h` carry the
configuration. Account selection and child environment changes belong to the
launcher. envoy never detects a launcher or falls back when one fails: a
fallback could spend on the very account the caller meant to avoid
(`EVIDENCE.md`, 2026-09-21). Process groups, pipes and session locks supervise
the resulting command unchanged.

The prefix is literal whitespace-separated argv, with complex setup left to a
wrapper executable. Every dispatch — a continuation, a retry, a fan-out member
— resolves its provider's current environment setting, so account choice stays
with the launcher and a recorded prefix pins nothing.

`meta.json.commandPrefix` records the resolved executable and prefix
arguments, `["codex"]` / `["claude"]` for the defaults. The executed argv is
`commandPrefix + providerArgv[1:]`, and `providerArgv` keeps its
provider-native meaning. The field is an optional observation, so it leaves
the schema where it is (`docs/records-and-collection.md` § Schema versions and
the stamp), and an absent field in an older record means unavailable.

A missing or refusing launcher is `infra`. After a successful spawn, an exit
without provider evidence leaves prompt state `unknown`: silence cannot prove
that no work ran, even when a launcher's stderr explains its refusal, and
stderr alone never fails a turn.

Claude transcript recovery — the evidence for the window before the first
stream event — is best-effort in envoy's own config directory. A launcher may
choose a child config directory envoy cannot know; a shared projects store can
preserve recovery, but the engine cannot infer one from command text.

## After a provider CLI upgrade

Re-check `--help` and the stream shapes before blaming a parser; `CLAUDE.md`
§ Provider CLI Drift names the versions the drivers were verified against.
When a codex turn goes quiet mid-turn, read codex's own rollout file before
suspecting envoy's pipes: it writes independently of them, and the mid-turn
stalls read so far were the codex client's own (`EVIDENCE.md`, 2026-08-03).

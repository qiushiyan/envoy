# envoy design

`CLAUDE.md` and the code say what envoy does; these docs record **why** — the
decisions, the alternatives they beat, and the settled non-goals a redesign
needs and cannot read from the code. This page holds what governs every part
of the engine and routes to a doc per domain. `EVIDENCE.md` is the dated log
of what each lesson cost: read it when a change would revisit a settled
question. A doc here states the position; the log records why it was paid for.

## A screen is not an API

envoy descends from a tmux-pane cross-reviewer that screen-scraped a provider
CLI, by way of `sidekick-runtime`, a Node engine whose lessons carry over
undiminished. The scraper's friction — completion watchers false-positiving on
input prompts, readiness checks timing out against splash screens, a human
hand-typing "seems it finished" — set the lesson that governs everything here:
sessions are driven headless and read back as data, JSON event streams and
result envelopes, never scraped output. Durable files as the return value,
semantic events as the runner's currency, and stdout as a deliberate interface
rather than a byproduct all follow from it.

The engine is a single Go binary on PATH, with a real provider seam rather than
provider conditionals, one owner for the record schemas so the writer (the
runner) and the reader (collect) cannot drift, and the stdlib closing the
process-lifecycle traps. **Zero third-party dependencies** is load-bearing: it
keeps the whole program auditable in one sitting and removes an entire class
of maintenance.

## Division of labor

The engine is deterministic mechanism: one turn in, files out. It holds no
judgment — no retries, no review loops, no gates, no provider selection, no
prompt templating. Callers (a human, an agent following a skill, a script) own
all of it: when to dispatch, what the prompt contains, how to verify what
comes back, whether to resume or retry. The split is why the engine can stay
small and its callers can be smart.

**Fold-back trigger:** if the engine ever wants retries, review loops, or
unattended-resilience machinery, it has outgrown its purpose — that
intelligence belongs in a caller. Don't grow the engine. A feature is admitted
when it is supervision and presentation and adds no turn semantics; the
fan-out is the feature that passed that test (`docs/fan-out.md`).

The adjacent non-responsibilities are settled too. envoy is **not a job
manager**: the caller's background-task layer owns liveness and completion
notification, and `pending` is only a recovery index over durable files for a
notification that may have been missed. envoy is **not a sandbox**:
`--allow-write` is intent, not enforcement (`docs/providers.md` § Permissions).

## Commitments that hold everywhere

- **Observed, never inferred.** The engine reports what it was asked (`model
  opus`, `(provider default)`) and what it observed, never what it concluded.
  Every sentence the engine says about itself is pinned to the instant and the
  channel it was read on: a stream read after teardown describes envoy's own
  cleanup, not the provider; raw bytes are not parseable events; and a store
  that could not be read is not an empty store, so discovery refuses instead of
  reporting a project with no jobs. How this binds models and effort:
  `docs/providers.md`.
- **Durable artifacts over stdout — and stdout still programs the caller.**
  Files are authoritative; stdout is a convenience view of them, read by an
  agent. Startup lines, progress vocabulary, collection blocks and recovery
  prose are prompts: facts, state and one proven next action, never protocol
  trivia to reinterpret. `next:` lines exist so the caller never derives its
  next move from status codes, and `internal/prose` owns every wording.
- **Background is the default posture.** Collection is notification-driven;
  the process exiting is the completion signal. A caller can sample durable
  usage observations while a turn runs, but a sample's age cannot establish a
  hang or a completion. There is no watch command and no `--detach` mode: a
  watched log once implied live evidence the stream did not carry, and a
  consult lost twenty minutes to a false hang diagnosis (`EVIDENCE.md`,
  2026-07-11).

## Where each decision lives

- **Job names, callers, generations, name holds and the store:**
  `docs/naming-and-storage.md`
- **What `meta.json` and `group.json` hold, schema versions, and how collect
  delivers a job:** `docs/records-and-collection.md`
- **A turn's lifecycle, the timeout, recovery by prompt state, session locks,
  continuing a conversation:** `docs/turn-lifecycle-and-recovery.md`
- **Several voices as one job:** `docs/fan-out.md`
- **Provider drivers, models and effort, permissions, transient errors,
  refusals, launchers:** `docs/providers.md`
- **Claude primary-turn context and token measurements (`meta.json.usage`):**
  `docs/primary-turn-usage.md`

## Deliberately not built

- **No daemon, status command, cancel service or job listing.** The caller's
  background-task layer is the live-job layer, and the caller named the job it
  wants; `pending` is the one index, and it is recovery, not discovery.
- **No alias translation, effort aliases, model fallbacks or provider
  auto-selection** (`docs/providers.md`).
- **No group-wide retry and no partial resume of a fan-out**
  (`docs/fan-out.md`).
- **No live steering of a running turn:** a supplement is a follow-up turn
  (`docs/turn-lifecycle-and-recovery.md` § Continuation).
- **No sandbox flag for codex, ever; for claude, no permission flag beyond the
  fixed bypass** (`docs/providers.md` § Permissions).
- **No activity-based hang detector and no timeout that resets on output**
  (`docs/turn-lifecycle-and-recovery.md`).
- **No prompt templating:** prompts arrive as files, whole — one per voice when
  the voices' questions differ.
- **No Windows support, and no migration from sidekick-era state.**

## Documentation bindings

`docs/documentation-standards.md` is the shared standard, copied verbatim from
its source (its first line says where); these bindings tie it to this
repository and win where they narrow it.

- **Hot path:** `CLAUDE.md` alone; `AGENTS.md` is a symlink to it, never
  written directly. It carries the ownership map and the invariants that
  prevent a wrong edit anywhere, each stated as its conclusion with the
  reasoning left here. This page is the landing page for design work.
- **Evidence:** `EVIDENCE.md` at the root is the dated log, one entry per
  incident or mining pass, appended by `/improve-tool`. Its entries are
  history and never rewritten; its header carries how to read them against
  the present surface. A live doc cites an entry as `EVIDENCE.md`, YYYY-MM-DD.
- **Proposals** are not kept: a shipped proposal is deleted and git holds it.
  The check block's `<live docs>` are the git pathspecs
  `':(glob)docs/*.md' CLAUDE.md README.md`.
- **Paths** are written from the repository root.
- **The protected set:** the caller-facing strings in `internal/prose` and the
  `envoy -h` page are a contract integration tests assert exactly, so a doc
  quotes them verbatim or not at all; the dated measurements in `EVIDENCE.md`;
  the commands in `CLAUDE.md` § Commands.
- **Own check:** no live doc presents a retired surface as current.

  ```bash
  git grep -nE '`envoy (fan|turn|steer|jobs)|--(coordinate-file|resume-from|with-from)|internal/(steer)|DESIGN\.md' -- ':(glob)docs/*.md' CLAUDE.md README.md
  ```

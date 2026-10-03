# Naming and storage

A job's address is a name its caller chose before dispatch. This doc is why
names work the way they do — generations, caller scope, holds, the fallback —
and where jobs live. The part of a record a name reads, its stamp, is
`docs/records-and-collection.md` § Schema versions and the stamp.

## The name is the address

The caller chooses the job's name before dispatch and collects by it, so
nothing printed by a dispatch whose stdout the harness hides has to be read
back. Other handoffs were tried and paid for first:

- **"The newest job in the store."** Name-ordered, printed before the first
  record, and racy against a second dispatch in the same project.
- **A coordinate file the dispatch wrote.** The file's path itself had to
  survive across the caller's shell calls, and the logs showed it did not
  (`EVIDENCE.md`, 2026-09-12).

A directory is reserved by creating it, so creation is the collision check,
and a refusal that ran nothing gives the name back.

## Generations: a directory per dispatch

Callers do not invent names: the skills that drive the engine hand every
session `review-r1` and `consult-r1`, so in a checkout that outlives one task,
reuse is the normal case. Every dispatch under a name gets a generation of its
own — `<name>`, `<name>+2`, `+3` — and a directory is an identity that never
moves, so every recorded `resumedFrom` and printed `resume:` path stays true.
`+` is outside the name pattern, so no caller can name a generation, and its
path reaches it.

Refusing a reused name is the alternative this beats. A name is ambiguous
across time whatever the engine does, and refusal only chooses the stale
reading: a refusal whose exit code a pipe swallowed went unseen, and `collect`
by the name served a nine-day-old review of other code as `ok` (`EVIDENCE.md`,
2026-09-21).

## Caller scope

`collect` and `@<job>` resolve a name to **the caller's own newest
generation**, else the newest of anyone's (`job.Resolve`). The second clause
lets a later session pick up an earlier one's work by name, and is what a
caller with no identity always gets.

Scoping is what makes reuse safe rather than merely likely-safe. Unscoped,
delivery would have to release a name, and delivery proves receipt, not that
the caller is done with the name: A collects `consult-r1`, B reuses it, and
A's `--with @consult-r1/codex` continues B's conversation, with no lock to
object because B's session is valid and free. Scoped, B's dispatch never
changes what the name means to A.

The caller is the session identity its harness exports (`job.CallerEnvKeys`:
`ENVOY_CALLER`, else Claude Code's `CLAUDE_CODE_SESSION_ID`), recorded as
`caller` in `meta.json` and `group.json` — an observation of the environment,
never derived, and absent where none was exported. codex-cli exports no
per-session variable to its shell, so a Codex caller keeps the unscoped
meaning unless something sets `ENVOY_CALLER` for it. The provider child does
not inherit the caller identity: it is a session of its own, and a job it
dispatches through envoy must not answer to its dispatcher's names.

## Name holds

A reused name is refused only by a job that still holds it, and only for
callers that share a meaning of the name. A dispatch takes the name away from
the job it means to this caller now, and from the newest job, which is what it
means to every caller without a generation of its own. Either one holds the
name while **undelivered** — running, or terminal and uncollected, the meaning
`collectedAt` has — against a caller it shares that meaning with: the same
caller, or either side having no identity. Callers that each have an identity
never contend. `collect.NameHold` is the one definition of a hold.

A job holds on evidence, never on absence, and each edge is decided by what
can be observed:

- **A fan-out member with no record** has nothing a collect could deliver, but
  its absence does not say it was refused: members start independently, and
  one was seen still preparing its turn after its sibling had finished and
  been collected. What says it never will start is the supervising process
  being gone, so `group.json` records `runnerPid` and a recordless member
  holds exactly while that process may be alive — which also frees a round in
  which every member was refused.
- **Another schema version's job** holds only while its own runner may be
  alive, decided from its stamp alone — what a fan-out's directory lists is no
  part of it. This engine can never deliver that job, so a longer hold would
  strand the name for good, and its refusal names the other version's process
  exiting as what frees the name, never a collect.
- **A store that exists but cannot be listed** is an error, never generation
  zero, and so is a generation newer than the caller's selected one whose
  record is there and cannot be read: either would let an older job answer to
  the name.
- **A reservation's first moments** have no record, so it cannot say whose it
  is, and a second caller dispatching the same name in that instant is refused
  rather than guessed about. This window is accepted.

Rejected alternatives:

- **Rotating the old directory aside** so the new job takes the bare path:
  every `resumedFrom` record and printed `resume:` command naming the old path
  would silently mean a different job.
- **An age threshold for "stale":** judgment, where delivery is an
  observation.
- **A scope flag the caller passes:** the model would have to know about
  collisions, which is the thing being removed.

## The fallback and its note

The fallback to the newest of anyone's is a reading, not an observation, and
the engine cannot tell its causes apart: work picked up on purpose, and a
caller whose identity changed mid-task — which owns no generation under its
new identity, so if another session reused the name meanwhile, the fallback is
that session's job.

Refusing an ambiguous fallback is rejected: in a checkout that outlives one
task nearly every name has several owners, so picking work up by name would
almost always be refused and callers would have to learn paths. Instead the
engine reports the one thing only it knows. A collect whose name fell back
carries a `note:` under the job it resolved to (on stderr for `--result-only`,
which is the payload alone), and a turn whose `resumedFrom` was dispatched by a
different recorded caller carries one under `resumed-from:` — rendered from
both records, so it appears wherever that job is read. A path never falls back,
and every `resume:` line hands one over.

## The store

Jobs live in one central store, `~/.local/state/envoy/jobs/<slug>/<name>/`,
never in the project tree: repo-local runtime directories (the predecessor's
`.sidekick/`) polluted every repo they touched. The slug is `basename-hash8`
(`job.ProjectSlug`): the basename for a human scanning the store, the hash of
the symlink-resolved path for uniqueness, and the git root as anchor, so a
dispatch from a subdirectory belongs to the project.

The store is an implementation detail by contract. The caller names the job;
`run`, `collect` and `pending` re-derive the project's store from the
directory they run in, and no caller constructs a path. A name is one path
segment; anything starting with `/`, `.` or `~` is a directory taken as given.

Creating the job directory is the reservation — `Mkdir`, never
stat-then-create — because turns dispatched in the same second once shared one
directory and overwrote each other's records (`EVIDENCE.md`, 2026-07-27). A
dispatch that loses the `Mkdir` to a concurrent one re-reads the name and
takes the next generation.

Session locks live beside the store, in `~/.local/state/envoy/locks/`
(`docs/turn-lifecycle-and-recovery.md` § Session locks).

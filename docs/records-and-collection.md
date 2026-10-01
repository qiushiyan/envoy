# Records and collection

A job's records are the truth about it, and `collect` is how that truth
reaches the caller. This doc is why records hold facts rather than words, how
schema versions are handled, and why a collected block is shaped the way it is.
`internal/job` owns the record types (`meta.go`, `failure.go`, `group.go`,
`usage.go`) for writer and reader alike.

## Records hold facts; collect renders

`meta.json` carries what the turn was dispatched with and what the engine
observed, never a rendered command or prescription. Every `resume:`, retry and
`next:` line is rendered at collect time from those fields plus the directory
the record was read from, so:

- the identical retry repeats the dispatch it replaces — the same source
  conversation, the archived prompt, the spend cap — under a new name;
- the resume line can never disagree with what dispatch would accept
  (`docs/turn-lifecycle-and-recovery.md` § Continuation).

Why a turn did not deliver is recorded the same way. `failure` keeps the cause
and the provider's own words and code — absent when the provider gave none,
never a fallback the engine wrote — and `sessionLockConflict` keeps the lock
file's account of the turn holding the session; `internal/prose` words both
whenever the record is read. Storing the sentence is the alternative this
beats: a persisted command drifts with every wording or verb change and cannot
be repaired without a compatibility layer, and a lock refusal carries an
`envoy collect` command, so a stored one would go on prescribing a verb after
the verb changed.

The archive is what was sent: `prompt.md` is written from the one read that
feeds the provider's stdin, never from a second read of the caller's file.

A record is also what makes a directory a job — `meta.json` for a turn,
`group.json` for a fan-out — so discovery keys on records, and a reserved name
released without one was never a job.

## Schema versions and the stamp

A record from another schema version is refused by name wherever its meaning
is read, never reinterpreted. The version is read before anything it governs,
so a field another schema shaped differently is refused as that schema, not as
a decode error.

The refusal stops short of naming. A name reads only a record's **stamp** —
for a turn `caller`, `status`, `collectedAt` and `runnerPid` (`job.Stamp`),
for a fan-out's manifest `caller` and `runnerPid` (`job.GroupStamp`) — fields
whose meaning no schema has changed, so a schema bump leaves every reused name
in a store resolvable. A meta schema bump run against a real store showed what
the rule prevents: every Claude Code caller reusing a name was refused, and
`pending` listed every collected job as damaged (`EVIDENCE.md`, 2026-10-01).

Another version's job holds its name only while its own engine may still be
running it (`docs/naming-and-storage.md` § Name holds). `pending` reads the
same stamp: a live job is skipped, a collected one owes nothing, and an
uncollected one is listed as another version's, never as damage. A fan-out's
roster changed shape between group schemas and is not part of its stamp, so
`pending` judges another version's fan-out by the records its member
directories hold.

Only a replaced field moves a schema. An optional observation (`usage`,
`commandPrefix`) keeps the version: existing fields and continuation
eligibility keep their meaning, and readers treat an absent field as
unavailable. Bumping for an addition would refuse compatible jobs and force
callers to change their schema pins; the engine version (`envoy version`)
identifies feature availability. `TestUsageSchemaCompatibility` pins this
boundary.

## Collection: delivery, not display

`collectedAt` means one thing: *the return value reached a caller*. The stamp
marks a delivered deliverable — an ok turn's printed result body, or a non-ok
turn's full diagnostic block, whose status and recovery are its result. The
block is rendered in full, written, and stamped only after the write
succeeded. `--status-only` and `pending` never stamp, since they read
coordinates without delivering anything, and neither does a collect of an ok
turn whose `result.md` will not read: the result stays owed and `pending`
keeps listing the job.

The selection flags exist because the block is read by an agent whose context
the result body and the status preamble compete for. They select sections and
never soften the contract: `--result-only` on anything other than an ok turn
prints the full block, because a non-ok turn's status and next action *are*
its result, and silence in their place would manufacture a payload that does
not exist.

## The tiered block

**The block is tiered on a narrower question than status: did the payload
land?** Settings, token counts, context usage, prompt-state evidence, result
kind and log paths form the diagnostic tier. A turn that reports ok *and*
whose `result.md` reads holds that tier back and prints what remains to act on
— coordinate, status, duration, follow-up commands, payload, next action.
Everything else prints the whole preamble, and `--status-only` prints it on
demand.

Note the asymmetry with the stamp: a non-ok turn's block *is* a delivered
deliverable and is stamped, yet it prints in full, because what it delivers is
the diagnosis. Keying the tier on status is wrong at both edges: an ok turn
whose `result.md` will not read is sent to `raw.log` by its own next line,
which needs the paths, and `--status-only` delivers nothing by construction.
Nothing is hidden either way, which is what makes the tiering safe: stdout is
the convenience view and the files are the truth.

Rejected, each a rule that would fire on the wrong cases (`EVIDENCE.md`,
2026-08-02):

- **Gating each log path on the recovery branch that names it.** The log
  streams are one affordance, and which one a branch emphasizes is not
  eligibility; per-branch rules buy one line on a job already in trouble at
  the price of a block whose shape no one can predict.
- **Printing the settings line when the provider's reported model differs
  from the requested one.** It looks like reporting a substitution, but `opus`
  against `claude-opus-5` is the provider's own alias resolution — the mapping
  this engine refuses to own — so inequality cannot tell resolution from
  substitution, and the exception would fire on the most ordinary dispatch
  there is.
- **A bare `session:` line beside a continuation.** The `resume:` command
  names the job whose records carry the session, so a bare id only invites
  hand-assembling a follow-up without the job's cwd and write intent.
  `session:` prints only when no `resume:` command is offered — a running
  turn, or one whose session may belong to another job.

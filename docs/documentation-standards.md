<!-- Shared standard. Source: https://github.com/qiushiyan/dotfiles/blob/main/docs/documentation-standards.md — copies are refreshed from it by skill-sync and a local edit is overwritten. To change a rule, propose the wording and its reason in this project's review; the source integrates it. -->

# Documentation standards

A doc's reader is a capable model about to change this repository. It reads
code readily, so a doc earns its place by giving it what the code cannot: why
the system is shaped this way and what must stay true. A sentence that a
behaviour-preserving refactor could make false describes the code, and the
code is its home.

Each project's **bindings** (its handoff or update-docs skill, or its docs
index) name its live docs, its always-read files and their ceilings, its
evidence folders and any extra checks. A binding narrows a rule or says where it
applies; it never asks for content this standard gives to the code.

## What to document

- **Boundaries and owners**: which part owns a fact or a decision, and what
  crosses each seam. A flow through several packages is written at its seams
  (what crosses, what must hold there), never as a tour of each hop.
- **Invariants**, each with what breaks and what guards it: a test, a type, a
  lint. An unguarded one says so.
- **Decisions**: what was chosen, what it beat, and what would reopen it, as
  the person who decided gave them; a reason nobody gave is asked, never
  inferred.
- **Routes**: where a reader starts for a question, as a pointer to the file
  or symbol that owns the detail.
- **Runbooks**: ordered actions with a checkable result and the exact
  commands.

Design docs hold what is true now. Proposals hold unbuilt work and open
questions; evidence (dated specs, records, research) holds how a decision was
reached, in dated files (`YYYY-MM-DD-name.md`). When a proposal ships, its
surviving decisions move into the design doc and the proposal stays a dated
record.

## What good docs look like

- **One home per meaning**; every other doc points at it, and every doc is
  reached from the index or an instruction file.
- **A cited number or heading is an address**: numbering never shifts, and a
  superseded entry keeps its number with a pointer to its successor.
- **Present tense, true at merge.** History lives in git and dated records;
  a sentence about unbuilt behaviour says it is planned. Whether something
  serves is asked of the running system.
- **Each claim names the file or symbol that owns its detail**, and the name
  resolves.
- **Sized to the reader's question**: a heading per question; prose for an
  argument and its reasons; a list for independent entries; a table only
  where rows are compared; a fenced block for anything run or grepped.
- **The always-read path** (the entry doc and instruction files) carries the
  vocabulary, the boundaries and the traps every session meets. Everything
  else is reached by a pointer that says when to follow it.

<example type="avoid">
The bound is the driver's (SETTLE_BOUND_MS, 25 s; the master clamps it to [0.5, 30] s, and its own DEFAULT_SETTLE_BOUND_MS, 8 s, serves only a hand-made POST).
</example>

<example>
A settle that spends its whole bound reads as expired, never refused; the activation driver owns the bound.
</example>

## When a change touches the docs

A change earns a doc edit when it changed what a later change owes: a
boundary or owner moved, an invariant appeared or stopped holding, a decision
was made or reversed, a route into the code moved. Find these in what the
work learned that the diff does not show: the review findings fixed, the
guards added, what the person decided, and what the first attempt got wrong.
Most changes do none of these, and the pass ends with no design-doc edit; a
record the bindings name, such as an owed read, is still written.

When a change makes a sentence false, apply the refactor test. A sentence
that described code is cut to the rule it carried, or deleted. A sentence
that states an obligation the code now breaks is a regression or a changed
design: ask which, never silently match either side.

A production read a change still owes is one marker in that change's own
record, `**Closing read owed** (#PR) — ready when: <condition>. Read:
<predicate>.`, closed by a verdict comment on that PR; nothing keeps a list
of the open set.

## Anti-patterns, and what replaces each

- **Code read aloud**: a symbol in parentheses after each clause, a
  constant's value, a log field, one function's steps, a list naming every
  member. Name the owner and the rule its members obey.
- **A cache of the environment**: a directory tree, a config's keys, a
  command's flags. Point at the source; keep only what a lookup cannot reach.
- **Changelog prose**: "now", "no longer", "added in #N", a dated status
  line. State the present; the change belongs in git.
- **A region every change writes**: a status page, a list of landings. Each
  change's facts live in its own record.
- **Sediment**: a description brought up to date each time the code moves.
  Cut it to its rule the first time it goes stale.
- **One doc as plan, log and reference at once.** Keep present, proposed and
  evidence in separate docs.

## Checks

`doc-check` (the update-docs skill's script) fails on a live doc that grew
without naming the obligation it now carries, a dead path or code name, and a
`§` reference that resolves nowhere, in either direction; it lists wording
candidates without failing; each candidate is fixed or kept for a stated
reason. Run it with the bindings' live-doc pathspecs before committing, and
re-read each changed doc whole.

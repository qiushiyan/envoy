<!-- Shared standard. Source: https://github.com/qiushiyan/dotfiles/blob/main/docs/documentation-standards.md — copies are refreshed from it by skill-sync and a local edit is overwritten. To change a rule, propose the wording and its reason in this project's review; the source integrates it. -->

# Documentation standards

**Write for the future, not the past.** A doc's reader holds the system's
present design well enough to carry it into their own work: what it is for,
how its parts relate, the constraints that bite, and why it is shaped this
way. That reader does not care what the system used to be.

The reader is a capable model that will inspect the repository and then
change it. Docs carry the **mental model**: ownership, boundaries,
invariants, decisions, and the traps the environment cannot reveal. Code,
config, directory listings, and `--help` own their inventories.

**Docs grow one sync at a time,** and every addition looks small where it
lands. A line earns its place by what a reader would get wrong without it.
A doc that gains lines sheds some; net growth needs a reason you can state,
such as a new subsystem or a new boundary.

Each project **binds** this standard in its documentation entry point (the
update-docs or handoff skill, or the docs index): its hot-path files and
ceilings, evidence directories, proposal retention, the base its paths are
written from, and its own checks. Where a binding narrows a rule, it wins.

## Structure that reads in plain source

An agent reads the file, not a rendering, so the hierarchy shows in plain
source, with no more structure than the content has:

- **a heading** per distinct reader question;
- **a sectioned list** for independent entries, above all "when X, read Y":
  a bold label, a colon, the entry;
- **a numbered list** where order matters;
- **prose** for an argument and its reasons, since bullets drop the
  connectives that carry it;
- **a table** only where the reader compares the same short attributes
  across rows;
- **a fenced block** for anything the reader runs or greps.

An agent reaches for a grid by reflex, so this is the rule most often
broken:

<example type="avoid">
```markdown
| The question concerns | Read |
| --- | --- |
| Cohort analytics, who is included, who may view | `docs/dashboard/README.md` — Mental Model, Audience and Auth, Data Sources |
| Admin preview or toolbar behavior | `docs/admin-tools.md` — Mental Model, Who Can Access, then Preview Mode |
```
</example>

<example>
```markdown
## Relevant knowledge

- **Cohort analytics, who is included, who may view:** `docs/dashboard/README.md` — Mental Model, Audience and Auth, Data Sources
- **Admin preview or toolbar behavior:** `docs/admin-tools.md` — Mental Model, Who Can Access, then Preview Mode
```
</example>

## Documentation shape

Each kind of content has one job, and a doc is one kind:

- **Design docs** say what is true today, in the present tense, edited in
  place. When a proposal ships, its surviving decisions fold in here, so no
  two docs describe one subsystem. A doc/code disagreement is a doc bug or a
  design regression, resolved explicitly, never by silently matching either
  side.
- **Runbooks** are ordered actions with a checkable result; exact commands
  live here.
- **Proposals** (specs, plans, roadmaps) are unbuilt work and open
  decisions. A live doc may cite one as a proposal but depends on none. Once
  shipped, its decisions live in the design doc, and the proposal is pruned
  or kept as a dated record as the bindings say.
- **Evidence tiers** (specs, issues, records, research), where a project
  keeps them, sit behind settled decisions: dated filenames
  (`YYYY-MM-DD-kebab-name.md`), deleted only after distilling, and edited
  after merge only for the marks the bindings name. An item earns
  prominence by being cited where its lesson applies; nothing keeps a
  roster.
- **An index** is a curated route, and every live doc is reachable from it.
  An unrouted doc goes unread and rots.

**An owed read lives in the record of the change that owes it.** When
production must still answer what a change's tests could not, its record's
`## Owed` carries one marker:

```markdown
**Closing read owed** (#PR) — ready when: <observable condition — #PR serving, an elapsed window, the first qualifying event>. Read: <predicate>.
```

The read is a comment on that PR opening with its verdict, then predicate,
window, and result. A confirming comment closes it; a read that overturns
something is new work with its own record. The open set is derived from the
markers and those comments, so nothing in the tree lists it.

**No region that every change writes.** A dated list of landings, a status
paragraph, a header each milestone rewrites: each is a second home for
facts that belong to one change's record, and the line every parallel
branch conflicts on. Where a shared doc must take per-change edits, each
change owns an entry no other change edits.

**A live initiative**, a system partly built, keeps status, proposal, and
present apart. Its README carries a standing **What is live** block, one
entry per module doc naming its sections that describe running code. Chosen,
disputed, and superseded decisions live in a decisions ledger; delivery
state (unbuilt, building, merged as #PR) lives on each milestone's own entry.
Whether something serves is asked of the running system. A cited number or
heading is an address: numbering never shifts, and a superseded entry keeps
its number with a pointer to its successor.

## The hot path

The hot path is what a session pays before choosing its work: the root
`CLAUDE.md` or `AGENTS.md`, a package-local instruction file, and the
landing page a root pointer names first. Its prose earns its bytes by
preventing a wrong edit: conclusions and pointers there, proof and recovery
detail on demand. The first-read budget is about **100 KB** (`wc -c`) unless
the bindings set a tighter one.

A first-read document keeps the vocabulary, the workflow, and the
load-bearing constraints. Mechanism one branch of work uses moves to a
satellite the spine names. Each section answers one question, so a session
reads to the depth of its question and never the whole file to be safe.

## What earns documentation

A cross-package flow or ownership boundary; an invariant whose violation
damages live state; a non-obvious decision and the alternative it beats; an
operational sequence a filename or command cannot supply; the evidence
needed to reproduce or retire a workaround. A compact relationship beats a
tour:

```text
symptom → owner → invariant → check
a run dies at claim time → the claim loop's deadline → a claim defers, it never blocks
                          → grep the journal for the deferral reason
```

What does not earn it:

- **Inventories.** Name a suite's responsibility, the few config surfaces
  that form a boundary, and a count only where the number is the invariant.
  A list in a live doc is a closed set with a stated criterion, never an
  append target.
- **Mechanism below the reader's action.** A tool is documented by what to
  run and what to do when it fails; a design by what it is for.
- **A tree edit for a file add or rename.** A directory tree names only what
  a reader must know exists.
- **A story in a lessons entry.** An entry states the invariant, the hazard
  in the present tense, the guard that pins it, and the record that bought
  it. An entry whose hazard is gone is deleted.

## Writing standards

- **Present state.** A live doc has no "added X" or "as of Y". The diff also
  leaks in disguised: "B, not A", "replaces A", "no longer". A predecessor
  earns a line only while it still bites, while a transition is mid-flight,
  or as the alternative the design beats when a reader might propose it
  again. When a change closes a gap, sweep the tree for the sentence that
  described it.
- **What this change can know.** A doc merges with its change, so it states
  what that change makes true. A merge sha, a serving time, or a read's
  result becomes true later; it is read from the running system, never left
  as a placeholder.
- **Current names.** Real, searchable nouns. Cite a file as a bare
  backticked path from the base the bindings name; leave signatures and
  option lists in source. Planned behavior is marked as planned.
- **Every edit re-reads the whole doc,** merges overlap instead of adding a
  second description, and folds new facts into the section they belong to.

## When docs need updating

- **None:** bug fixes, internal refactors, tests, dependency bumps.
- **Module-level:** a new function, flow, or option inside a subsystem; the
  one doc that owns it.
- **Architecture-level:** a new subsystem, boundary, integration, or policy;
  the index, perhaps a new doc, a proposal distilled, and the doc structure
  reconsidered, not patched at the point of change. A branch that straddles
  tiers takes the higher.

## Before you commit a doc change

The violations below are invisible at the point of writing. Where the
`update-docs` skill is installed, its `scripts/doc-check` runs these checks
on the staged docs, resolves every `§` reference, and prints each doc's
growth. Otherwise stage the docs (`git add -- <paths>`) and run the block,
passing each live-doc pathspec from the bindings as its own quoted argument
(an unsplit list matches nothing and passes silently):

```bash
# every `<doc>.md § Heading` you touched resolves
grep -rn '§ <the heading>' --include='*.md' . ; grep -n '^#\+ <the heading>' <the cited doc>

# cardinal numbers ("three" → "four" is not the fix; name the members)
git diff --cached -- '*.md' | grep -nE '^\+.*\b(two|three|four|five|six|seven) [a-z]+'

# changelog wording
git diff --cached -U0 -- '*.md' | grep -E '^\+[^+]' \
  | grep -nE 'no longer|previously|used to|formerly|before this|was replaced|is now'

# PR numbers, dates as narrative
git diff --cached -U0 -- '*.md' | grep -E '^\+[^+]' | grep -nE '#[0-9]{3,}\b|\b(since|as of|on) 20[0-9]{2}-|\(20[0-9]{2}-[0-9]{2}-[0-9]{2}\)'

# new tables
git diff --cached -U0 -- '*.md' | grep -nE '^\+\s*\|?\s*:?-+:?\s*(\|\s*:?-*:?\s*)+\|?\s*$'

# live docs leaning on proposals or evidence
git grep -nE '(specs|plans|proposals|issues|records|research|adr)/[^ )]*\.md' -- '<live doc pathspec>' ...

# facts from after the merge, or placeholders for them
git diff --cached -U0 -- '*.md' | grep -E '^\+[^+]' | grep -niE 'merged [^.]{0,24} as `?[0-9a-f]{7,}|serving (on|from|since) |to be filled|at (the )?(merge|closeout)'

# owed reads not written as the marker
git diff --cached -U0 -- '*.md' | grep -E '^\+[^+]' | grep -i 'read owed' | grep -vE '\*\*Closing read owed\*\* \(#([0-9]+|PR|N)'
```

What a hit means:

- **A narrative hit** (cardinal, changelog, PR, date): fine in a record or a
  dated measurement the bindings protect; in a design doc, a sentence to
  rewrite in the present tense.
- **A table:** stays when the reader compares cells across rows, otherwise
  becomes a sectioned list.
- **A reference into a proposal or evidence directory:** fine when the
  target is cited in its role; a defect when a live doc leans on unbuilt
  work.
- **A post-merge fact or owed wording without a marker:** move it to the
  change's record, as the marker.
- **Growth:** a doc that grew states why in the change's report.

Then re-read each modified doc as one narrative, and grep live docs for each
moved path and superseded term.

## The standards file itself

Rules accrete one incident at a time. A new rule enters as a check or a
worked example first, and as prose only when neither can carry it; a rule
that exists and was still broken gets a check, not a second statement. The
file stays under ~14 KB. Review it after a major model release: guardrails
written for an older model become friction for a newer one.

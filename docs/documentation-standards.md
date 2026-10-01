<!-- Shared standard. Source: https://github.com/qiushiyan/dotfiles/blob/main/docs/documentation-standards.md — copies are refreshed from it by skill-sync and a local edit is overwritten. To change a rule, propose the wording and its reason in this project's review; the source integrates it. -->

# Documentation standards

**Write for the future, not the past.** Every doc has one goal: a reader who holds the system's present design — its philosophy, its methods, the reasons it is shaped this way — well enough to carry them into their own work. That reader does not care what the system used to be; they care why it is the way it is now. A predecessor earns a line only as the present-state rule below allows.

The reader is a smart model that will inspect the repository and then change it. Documentation carries the **mental model** — ownership, boundaries, invariants, decisions, and the traps the environment cannot reveal; code, config, directory listings and `--help` own their inventories.

This file is the shared standard every project carries verbatim; each project **binds** it in its documentation entry point (the update-docs or handoff skill, or the docs index), naming its hot-path files and ceilings, evidence directories and records, proposal retention, the base its paths are written from, and its own checks. Where the bindings narrow a rule, they win.

## Structure that reads in plain source

This is the rule most often broken, because an agent reaches for a grid by reflex. **Structure follows how the reader uses the information.** The hierarchy is visible in plain source — an agent reads the file, not a rendering — and there is no more structure than the content has:

- **a heading** per distinct reader question;
- **a sectioned list** for independent entries, above all "when X, read Y" — a bold label, a colon, the entry — nested one level only where an entry has subordinate detail;
- **a numbered list** where order matters;
- **prose** for an argument and its reasons, since bullets drop the connectives that carry the reasoning;
- **a table** where the reader compares the same short attributes across alternatives, cell by cell;
- **a fenced block** for anything the reader runs or greps.

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

The rows were independent lookups, so the grid was carrying a list; the heading now names the reader's question and each entry reads whole in source.

## Documentation shape

Each kind of content has one job, and a doc is one kind:

- **Design docs** say what is true today — present tense, edited in place. When a proposal ships, its surviving decisions fold in here, so no two docs describe one subsystem. Docs lead, code follows: a doc/code disagreement is a doc bug or a design regression, resolved explicitly, never by silently matching either side.
- **Runbooks** are ordered actions with a checkable result; exact commands live here, not in a design summary.
- **Proposals** (specs, plans, roadmaps) are explicitly unbuilt work and open decisions; a live doc cites one as a proposal, and depends on none. Once shipped, its decisions live in the design doc and the proposal is pruned or kept as a dated record as the project's bindings say — either way nothing live routes through it.
- **An owed read lives in the record of the change that owes it.** When production must still answer what a change's tests could not, its own record (a spec's or issue record's `## Owed`) carries one marker naming the PR:

  ```markdown
  **Closing read owed** (#PR) — ready when: <observable condition — #PR serving, an elapsed window, the first qualifying event>. Read: <predicate>.
  ```

  The condition lets a later session take the read without the writer's memory. The read is a comment on that PR opening with its verdict, then predicate · window · result. A confirming comment closes the read and the marker stays as the record's history; a read that overturns something is new work with its own change and record. The open set is derived from the markers and those comments, so nothing in the tree lists it.
- **Evidence tiers** (specs, issues, records, research), where a project keeps them, are reference behind settled decisions: dated filenames (`YYYY-MM-DD-kebab-name.md`), deleted only after distilling, edited after merge only for the marks the project's bindings name. The filename is the index entry, nothing keeps a roster, and an item earns prominence by citation from the live doc where its lesson applies.
- **An index** is a curated route, and every live doc is reachable from it: an unrouted doc is invisible to readers and to a diff-scoped update, and rots.

**No region that every change writes.** A dated list of landings, a narrative status paragraph, a header each milestone rewrites: each is a second home for facts that belong to one change's record, and the line every parallel branch conflicts on. Where a shared doc must take per-change edits, each change owns an entry no other change edits.

**A live initiative** — a tree for a system that is partly built — keeps status, proposal and present apart: the README header carries a standing **What is live** block, one entry per module doc naming its sections that describe running code; what does not run yet is a slice's spec, folded in at merge. Epistemic state (chosen, disputed, superseded) lives in a decisions ledger, delivery state (unbuilt, building, merged as #PR) on each milestone's own entry; whether it serves is asked of the running system. A number or heading is an address once cited: numbering never shifts, and a superseded entry keeps its number with a pointer to its successor.

## The hot path

The hot path is what a session pays before it chooses its work: the root `CLAUDE.md` / `AGENTS.md`, a package-local instruction file while working there, and the landing page a root pointer names first. Hot prose earns its bytes by preventing a wrong edit: conclusions and pointers there, proof and recovery detail on demand. The first-read budget is about **100 KB** (`wc -c`) unless the project's bindings set a tighter ceiling; measure a changed hot document by section, and a section that became mostly mechanism is a split candidate however small the file.

**Spine first, one home per meaning.** A first-read document keeps the vocabulary, the workflow and the load-bearing constraints; mechanism used by one branch of work moves to a satellite the spine names, and a summary points at its owner rather than retelling it. **A section answers one question, and a fact is findable by the question that needs it**, so a session reads to the depth of its question and never the whole file to be safe.

## What earns documentation

A cross-package flow or ownership boundary; an invariant whose violation damages live state; a non-obvious decision and the alternative it beats; an operational sequence a filename or command cannot supply; the evidence needed to reproduce or retire a workaround. Prefer a compact relationship an agent can execute over a tour:

```text
symptom → owner → invariant → check
a run dies at claim time → the claim loop's deadline → a claim defers, it never blocks
                          → grep the journal for the deferral reason
```

What does not earn it:

- **Inventories.** Name a suite's responsibility rather than its ordinal, the few config surfaces that form a boundary rather than every option, and a count only where the number is itself the invariant. Every list in a live doc is a closed spotlight set with a stated criterion, never an append target.
- **Mechanism below the reader's action.** A tool the reader runs is documented by what to run and what to do when it fails, not how it works inside; a design by what it is for and the purposes it serves, not how each function or query is written.
- **A tree edit for a file add or rename.** A directory tree is a mental-model device naming what a reader must know exists.
- **A story in a lessons entry.** An entry is a seam guard: the invariant (bold, one sentence), the hazard in the present tense, the guard that pins it (a test or symbol), the record that bought it — and one line pointing at the test when a named test already pins it. An entry whose hazard is gone is deleted.

## Writing standards

- **Present state.** A live doc has no "added X", "as of Y". The diff leaks in with a present-tense disguise — "B, not A", "replaces A", "no longer" — every word true, the sentence shaped like the change. The **future-need test** for any trace of the before-state: will a reader who never saw A need it? Usually not; A earns a mention only while it still bites today, stated as a present hazard, while a transition is mid-flight, or when a reader might propose it again — then it is the alternative the design beats, kept with its reason. When a change closes a gap, sweep the tree for the sentence that described the gap.
- **What this change can know.** A doc merges with the change that writes it, so it states what that change makes true. The PR's number is known before merge and is cited in the change's record; a merge sha, a serving time or a read's result becomes true later and is read from the running system or the PR's thread — a placeholder for one is a second PR waiting to be written.
- **Current names.** Real searchable nouns; point at source with a line-sized description and leave signatures and option lists in source. Cite a repository file as a bare backticked path from the base the bindings name — an agent opens the path with its read tool, and link syntax adds nothing it can use. Planned behaviour is marked (a status line, a spec, an open question), never stated as fact; a merged change awaiting its production read is stated as built, and its owed marker carries the doubt.
- Every edit re-reads the whole doc, merges overlap instead of adding a second description, and folds new information into the section it belongs in — a doc that gains ten lines should usually shed five.

## When docs need updating

- **None** — bug fixes, internal refactors, tests, dependency bumps.
- **Module-level** — a new function, flow or option inside an existing subsystem: the one doc that owns it.
- **Architecture-level** — a new subsystem, boundary, integration or policy: the index, possibly a new doc, a proposal distilled — and the doc *structure* reconsidered, not a wording patch at the point of change. A branch that straddles tiers takes the higher. At any tier, a change only production can confirm writes its owed-read marker.

## Before you commit a doc change

The violation is invisible at the point of writing. Stage the docs this change touched (`git add -- <paths>`, so a new doc is diffed too), then run the block; `<live docs>` is the set the project's bindings name:

```bash
# every `<doc>.md § Heading` you touched, or that names a doc you renamed in, resolves
grep -rn '§ <the heading>' --include='*.md' . ; grep -n '^#\+ <the heading>' <the cited doc>

# no cardinal number entered a live doc ("three" → "four" is not the fix; name the members)
git diff --cached -- '*.md' | grep -nE '^\+.*\b(two|three|four|five|six|seven) [a-z]+'

# no changelog disguise entered a live doc
git diff --cached -U0 -- '*.md' | grep -E '^\+[^+]' \
  | grep -nE 'no longer|previously|used to|formerly|before this|was replaced|is now'

# no PR number, date-as-narrative or confidence boilerplate entered a design doc
git diff --cached -U0 -- '*.md' | grep -E '^\+[^+]' | grep -nE '#[0-9]{3,}\b|\b(since|as of|on) 20[0-9]{2}-|\(20[0-9]{2}-[0-9]{2}-[0-9]{2}\)'

# every new table is a review candidate
git diff --cached -U0 -- '*.md' | grep -nE '^\+\s*\|?\s*:?-+:?\s*(\|\s*:?-*:?\s*)+\|?\s*$'

# every live-doc reference into a proposal or evidence directory: what role does the target play?
git grep -nE '(specs|plans|proposals|issues|records|research|adr)/[^ )]*\.md' -- '<live docs>'

# no fact from after the merge, and no placeholder for one, entered a doc
git diff --cached -U0 -- '*.md' | grep -E '^\+[^+]' | grep -niE 'merged [^.]{0,24} as `?[0-9a-f]{7,}|serving (on|from|since) |to be filled|at (the )?(merge|closeout)'

# an owed read is a marker naming its PR
git diff --cached -U0 -- '*.md' | grep -E '^\+[^+]' | grep -i 'read owed' | grep -vE '\*\*Closing read owed\*\* \(#([0-9]+|PR|N)'
```

What a hit means:

- **A narrative grep** (cardinal, changelog, PR or date): fine in a record, an evidence tier or a dated measurement the bindings protect; in a design doc, a sentence to rewrite in the present tense with the evidence cited by record.
- **A table:** stays when the reader compares cells across rows; becomes a sectioned list when its rows are independent lookups; exempt inside a quoted avoid-example.
- **A reference into a proposal or evidence directory:** fine when the target is cited in its role — a proposal as unbuilt, a retained record or decision as evidence, an authoring guide as a guide; a defect when a live doc leans on unbuilt work.
- **A post-merge fact:** cite the PR by number in the change's record; the running system answers the rest.
- **Owed wording without a marker:** rewrite it as the marker in the change's own record.

Then run the project's own checks from its bindings, re-read each modified doc as one narrative, and grep live docs for each moved path and superseded term.

## The standards file itself

Rules accrete one incident at a time. A new rule enters as a line in the check block or a worked example first, and as prose only when neither can carry it; a rule that exists and was still broken gets a check, not a second statement. The file stays under ~14 KB; growth past that means a project binding leaked in or a rule is stated twice. Review it and the doc skills after a major model release: guardrails written for an older model become friction for a newer one, and removing stale guidance weighs the same as adding new.

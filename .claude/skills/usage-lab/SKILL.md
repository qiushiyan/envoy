---
name: usage-lab
description: "Mine envoy's real usage — the central job store and session transcripts — for evidence of bugs, missing affordances, and ops lessons."
disable-model-invocation: true
---

# Usage lab — read the store before inventing features

envoy's own job store is its usage lab: every dispatch leaves durable
artifacts, and every caller was an agent whose transcript survives. This
skill mines both for evidence, then routes each finding to the place that
retires it. DESIGN.md's evidence log is the cursor — mine from its last
dated entry forward, and end by advancing it.

## 1. The store pass

Summarize every job under `~/.local/state/envoy/jobs/<project>/` into
compact rows — status, promptState, provider/model, duration against its
cap, errorText, collectedAt (field truth: `internal/job/meta.go` and
`group.go`). Fan-outs nest: `group.json` sits in the job dir and member
`meta.json` files sit **one level deeper**, so glob both depths. Read a few
`group.json` files whole — contract strings recorded there (watch/resume
commands) have carried bugs the summaries hide.

Done when every job dir since the cursor appears in the summary and every
non-`ok` status or unusual duration has been read at the meta level.

## 2. The transcript pass

Use the obelisk skill (`~/dotfiles/claude/.claude/skills/obelisk/SKILL.md`)
for retrieval mechanics; the query that pays first is every Bash tool call
invoking envoy, chronological, with its error results:

```sql
SELECT tc.input_json, m.timestamp, s.project, tr.is_error, tr.content
FROM tool_calls tc
JOIN messages m ON m.uuid = tc.message_uuid
JOIN sessions s ON s.id = tc.session_id
LEFT JOIN tool_results tr ON tr.tool_use_id = tc.id
WHERE tc.name = 'Bash' AND tc.input_json LIKE '%envoy %'
ORDER BY m.timestamp
```

The command *shapes* are the evidence: flags chosen, dispatches chained with
`;`/`&`, collect piped through anything, coordinates recovered by grep/ls.
For each suspicious shape, pull the surrounding assistant text — the agent
usually narrates the workaround it is performing.

Done when every envoy invocation since the cursor is listed and each
non-plain shape has its surrounding narration read.

## 3. Read for friction

An all-`ok` store is itself a finding — it means the friction lives in the
caller loop, not the lifecycle. What converts to findings:

- **Workarounds**: shell post-processing of envoy output, chained or raced
  dispatches, hand-reconstructed coordinates. Each one is a caller paying
  for a missing mechanism.
- **Repetition**: the same pattern hand-rolled across sessions — especially
  hand-rolled *differently* each time — is the strongest affordance signal.
- **Drift**: a flag the installed binary rejected, prose the skills quote
  that the engine no longer prints.
- **Margins**: paths real usage never exercises (timeouts, locks, recovery)
  — evidence about headroom, not dead code.

## 4. Route every finding

Each finding gets exactly one class and destination:

- **Bug** → fix plus a regression test that fails on the old code.
- **Affordance** → a new mechanism, only if it stays supervision and
  presentation — judge it against DESIGN.md's fold-back trigger before
  building; judgment stays in callers.
- **Ops lesson** → an evidence-log entry only; no code.

Done when no finding is left unclassified, and nothing was built that the
evidence didn't pay for.

## 5. Advance the cursor

Write the dated evidence-log entry in DESIGN.md — what the logs showed,
what it cost, what changed — and give any new caller-facing behavior its
exact-string integration test. Done when the log's newest entry covers this
pass.

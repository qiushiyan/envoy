# Primary-turn usage

`meta.json.usage` lets a caller read Claude's primary-turn measurements without
interpreting the provider stream. The Claude driver owns attribution,
deduplication and reconciliation (`internal/provider/claude_usage.go`);
`KindUsage` events carry independent snapshots that the runner publishes
atomically as they arrive, without waiting for a heartbeat; and
`internal/job/usage.go` owns the record. The measurement adds no lifecycle
decisions: callers own any policy about context pressure.

## Scope

The scope is one job's primary turn. A continuation starts fresh counters and
a fresh peak, even though its first request includes earlier conversation
context. Fan-out members keep their own measurements. Codex leaves `usage`
absent, and absence in an older record likewise means unavailable. The
`tokens` block is separate: the provider-reported totals written at
finalization.

## The fields

- **Context and freshness.** `latestContextTokens` is the last valid
  response's input plus cache-read plus cache-creation tokens.
  `peakContextTokens` is the largest such sample in this turn; compaction can
  lower latest without lowering peak. `sampledAt` is envoy's receipt time for
  that sample. Neither count includes subsequent generated output or tool
  results, so it cannot predict the next request's size.
- **Response accounting.** `responses` counts distinct attributed primary
  message IDs, even when a response lacks usable input usage. Repeated IDs
  change nothing, including the sample time. Non-null `parent_tool_use_id`
  excludes a record before deduplication. `inputTokens`,
  `cacheReadInputTokens` and `cacheCreationInputTokens` accumulate valid input
  measurements; `outputTokens` stays null until terminal reconciliation accepts
  a complete output total. Missing or invalid measurements remain unknown,
  never zero.
- **Attribution.** `attribution` starts `unknown`; the reported session model
  establishes `complete` attribution until a response has missing or differing
  model evidence. Such a response leaves the last valid sample intact and marks
  attribution `incomplete`; `unexpectedModel` holds the last differing model.
  Later valid samples advance the measurement, but the issue persists. The
  comparison uses provider-reported models, never a requested alias.
- **Window and terminal totals.** `contextWindowTokens` stays null until the
  terminal record gives a positive window for the session's reported model. It
  is the only figure taken from `modelUsage`, whose totals include work outside
  the primary loop. `terminalTokens` preserves the terminal `result.usage`
  fields as `input`, `cacheRead`, `cacheCreation` and `output`, with missing or
  invalid fields null. Complete totals are accepted only when they agree with
  observed inputs and attribution permits reconciliation. Missing fields,
  budget stops, zeroed crash usage or contradictory totals preserve observed
  inputs and leave full output unknown. Terminal totals cannot reconstruct a
  missing context sample or peak.
- **Finality and completeness.** `state` is `unmeasured` until a valid context
  sample, then `live`. Once `final` is true, it is `settled` when there are no
  issues, otherwise `incomplete`. `issues` retains distinct evidence gaps;
  live measurements can already carry issues. Finality is independent of job
  success and process exit: a failed turn can have complete usage, and usage
  can settle while process cleanup continues. A missing window leaves the
  block incomplete even when output is known.

## Finalization

Runner finalization without a terminal record retains the live figures and
adds `missing_terminal`. An envoy process killed before finalization can leave
a non-final snapshot indefinitely; metadata alone cannot prove liveness.
Collection marks unfinished usage incomplete when it proves abandonment,
preserving any measurement already finalized by a terminal record.

Accounting is incremental — constant work per stream record, never a reparse
of stream history — and a snapshot has a fixed structure and a finite issue
vocabulary. Adding `usage` left the meta schema where it was
(`docs/records-and-collection.md` § Schema versions and the stamp).

The captured evidence behind the accounting is `EVIDENCE.md`, 2026-09-14: a
sanitized Claude Code 2.1.270 capture at
`internal/provider/testdata/claude-2.1.270-usage.jsonl`, pinned by
`TestClaudeUsageCapture`, with `TestClaudeUsageLiveAndCollected` pinning
event-driven persistence and display.

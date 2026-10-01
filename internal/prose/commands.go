package prose

import (
	"fmt"
	"strings"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/text"
)

// promptPlaceholder is the slot every follow-up command leaves open: a resumed
// turn needs a NEW prompt, and only the caller knows which file that will be.
// jobPlaceholder is the other slot: a follow-up is a new job, and its name is
// the caller's to choose. Commands are always rendered from structured fields
// — never edited as strings afterwards — so a path that happens to contain
// this text can never collide with the slot.
const (
	promptPlaceholder = "<your-follow-up.md>"
	jobPlaceholder    = "<new-job-name>"
)

// ContinueCommand is the complete command that continues the conversation(s)
// a finished job holds — one turn's session, or every member of a fan-out —
// as a new job. The records supply provider, session, model, effort, write
// intent, tree and baseline; the cap is phase policy and is carried from the
// job so the caller sees the number it will get. The prompt file is a
// placeholder: a continuation needs a new prompt, never the original again.
func ContinueCommand(dir string, timeoutMin float64) string {
	return fmt.Sprintf("envoy run %s --with @%s --timeout-min %g --prompt-file %s",
		jobPlaceholder, text.ShellQuote(dir), timeoutMin, promptPlaceholder)
}

// voiceSpec spells a member the way a caller types it: provider[:model[:effort]].
// An effort without a model keeps the middle slot empty (codex::high).
func voiceSpec(provider, model, effort string) string {
	switch {
	case effort != "":
		return provider + ":" + model + ":" + effort
	case model != "":
		return provider + ":" + model
	}
	return provider
}

// RedispatchCommand sends this job's prompt again as a new job — the
// follow-up for a prompt the provider provably never received, where
// re-running is safe, under a new name so this record stays collectable by
// name. It repeats the dispatch it replaces: the same conversation when the
// turn was a continuation (a
// cold voice would start a different one), the recorded tree, anchor, write
// intent, cap and spend cap, and the prompt exactly as the job archived it.
// The new dispatch reads its own environment, including the launcher prefix;
// CommandPrefix is an observation, not a setting this command replays.
func RedispatchCommand(dir string, m *job.Meta) string {
	voice := voiceSpec(m.Provider, job.Deref(m.Model), job.Deref(m.Effort))
	if m.ResumedFrom != nil {
		voice = "@" + text.ShellQuote(*m.ResumedFrom)
	}
	parts := []string{"envoy run", jobPlaceholder, "--with " + voice,
		"--prompt-file " + text.ShellQuote(job.Workspace{Dir: dir}.PromptPath())}
	if m.AllowWrite {
		parts = append(parts, "--allow-write")
	}
	if m.GitBaseline != nil {
		parts = append(parts, "--baseline "+*m.GitBaseline)
	}
	if m.Cwd != "" {
		parts = append(parts, "--cwd "+text.ShellQuote(m.Cwd))
	}
	if m.MaxBudgetUSD != nil {
		parts = append(parts, fmt.Sprintf("--max-budget-usd %g", *m.MaxBudgetUSD))
	}
	parts = append(parts, fmt.Sprintf("--timeout-min %g", m.TimeoutMin))
	return strings.Join(parts, " ")
}

// CollectCommand prints one job as a single block; it is the caller's read
// path for every turn, healthy or not.
func CollectCommand(outDir string) string {
	return "envoy collect " + text.ShellQuote(outDir)
}

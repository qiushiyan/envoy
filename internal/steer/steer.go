// Package steer holds every sentence envoy addresses to its caller.
//
// The caller is usually an AI agent reading stdout, so this text is a prompt
// surface: it must say what happened, what that rules in or out, and the one
// action to take next — and it may only prescribe what the engine actually
// observed. Centralizing it here keeps a single wording for each situation
// (the runner, the drivers, and collect all describe the same few outcomes)
// and makes the whole vocabulary reviewable in one file.
package steer

import (
	"fmt"
	"strings"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/text"
)

// Turn is what a dispatched turn needs to be rebuilt as a command.
type Turn struct {
	Provider   string
	SessionID  string
	Cwd        string
	Model      string
	Effort     string
	AllowWrite bool
	TimeoutMin float64
}

// ResumeCommand is the complete command that continues this provider session,
// carrying the original turn's settings so a follow-up cannot silently drop
// write intent or run against another tree. The prompt file is a placeholder:
// a resumed turn needs a new prompt, never the original one again.
func (t Turn) ResumeCommand() string {
	if t.SessionID == "" {
		return ""
	}
	parts := []string{"envoy turn", "--provider " + t.Provider, "--resume " + t.SessionID}
	if t.Model != "" {
		parts = append(parts, "--model "+t.Model)
	}
	if t.Effort != "" {
		parts = append(parts, "--effort "+t.Effort)
	}
	if t.AllowWrite {
		parts = append(parts, "--allow-write")
	}
	if t.Cwd != "" {
		parts = append(parts, "--cwd "+text.ShellQuote(t.Cwd))
	}
	parts = append(parts, fmt.Sprintf("--timeout-min %g", t.TimeoutMin), "--prompt-file <your-follow-up.md>")
	return strings.Join(parts, " ")
}

// CollectCommand prints one job as a single block; it is the caller's read
// path for every turn, healthy or not.
func CollectCommand(outDir string) string {
	return "envoy collect " + text.ShellQuote(outDir)
}

// Recovery prescribes the next move from the prompt state alone — the only
// thing the engine can actually prove about a turn that did not return a
// result. remedy is an optional cause-specific clause the caller must handle
// first (an exhausted budget cap, say); "" when the cause needs no fixing.
//
// The three states carry different licenses, and confusing them is the
// expensive mistake: re-sending a prompt the provider already accepted
// duplicates work that may already have changed the tree.
func Recovery(promptState, resumeCmd, remedy string) string {
	fix := ""
	if remedy != "" {
		fix = " " + strings.TrimSpace(remedy)
	}
	switch promptState {
	case job.PromptAccepted:
		if resumeCmd == "" {
			return "The provider accepted this prompt, so work may already exist in the working tree." + fix +
				" Read result.md, progress.log, and the tree to see how far it got before deciding anything else."
		}
		return "The provider accepted this prompt, so work may already exist in the session and the working tree." + fix +
			" Read result.md and check the tree, then continue the same session with a follow-up prompt:\n  " + resumeCmd +
			"\nSending the original prompt again would repeat work that already happened."

	case job.PromptNotStarted:
		return "The provider never started, so this prompt did not run and nothing was changed." + fix +
			" Re-run the identical command once; if it fails the same way, the provider CLI itself is the problem to report."

	default: // unknown
		body := "Whether the provider began work is unproven — silence is not proof that nothing ran." + fix +
			" Read progress.log, raw.log, stderr.log, and the working tree."
		if resumeCmd == "" {
			return body + " Re-dispatch only once they show the prompt never began."
		}
		return body + " If any of them show work, continue the same session with a follow-up prompt:\n  " + resumeCmd +
			"\nRe-dispatch the original prompt only once they show it never began."
	}
}

// Orphaned describes a job whose runner is gone while its provider survives.
// Acting now would put two turns in the same tree, so this outranks whatever
// the prompt state would otherwise license.
func Orphaned() string {
	return "A process from this job's provider group is still alive and may still be writing to the working tree. " +
		"Wait for it to finish or stop it first — resuming or re-dispatching now would leave two turns editing the same files. " +
		"Once it is gone, collect this job again for the recovery action."
}

// Unprovable is the honest answer when liveness itself could not be read.
func Unprovable() string {
	return "This job's process state cannot be read, so neither completion nor abandonment is established. " +
		"Read progress.log, raw.log, stderr.log, and the process list before resuming or re-dispatching."
}

// LockedSession explains a session-id collision, which stops a turn rather
// than letting two live turns interleave one conversation.
func LockedSession(detail string, providerAlive bool) string {
	warning := ""
	if providerAlive {
		warning = " This job also has a provider process that may still be alive; stop or wait for it first."
	}
	return fmt.Sprintf("Another turn holds this session id: %s.%s Collect the job named there before touching this session — "+
		"two live turns on one conversation corrupt it.", strings.TrimRight(detail, ". \t\n"), warning)
}

// StatusGloss says what a terminal status rules in or out, so the caller need
// not carry envoy's status table to act on one word.
func StatusGloss(status string) string {
	switch status {
	case job.StatusOK:
		return ""
	case job.StatusFailed:
		return "the provider ran and reported a failure"
	case job.StatusInfra:
		return "envoy or the environment failed, not the model"
	case job.StatusTimeout:
		return "the wall-clock cap elapsed, which is not evidence the provider hung"
	case job.StatusInterrupted:
		return "a signal stopped the turn"
	case job.StatusAbandoned:
		return "the process ended without publishing a result"
	default:
		return ""
	}
}

// StatusLine renders a status with its gloss.
func StatusLine(status string) string {
	if gloss := StatusGloss(status); gloss != "" {
		return fmt.Sprintf("%s — %s", status, gloss)
	}
	return status
}

// DispatchNext is the nudge printed the moment a turn starts, when the caller
// is deciding whether to wait, poll, or walk away.
func DispatchNext(outDir string) string {
	return "let this command run to completion, then collect the job: " + CollectCommand(outDir) +
		" · tailing the logs is observation only, and output going quiet never means finished"
}

// RunningNext is what a caller that collects too early should do instead.
func RunningNext(outDir, watchCommand string) string {
	line := "This turn is still running. Wait for its process to exit, then collect it again: " + CollectCommand(outDir) + "."
	if watchCommand != "" {
		line += " Watching progress meanwhile is fine (" + watchCommand + "), but a quiet log is not completion."
	}
	return line
}

// CollectedOK closes a successful collection by pointing at the payload.
func CollectedOK() string {
	return "result.md above is this turn's return value — use it in the step that dispatched it."
}

// CollectThisJob is the action every terminal turn ends on.
func CollectThisJob(outDir string) string {
	return "Collect and verify this job: " + CollectCommand(outDir) + "."
}

// Stopping is what to do while a stop is in flight.
func Stopping() string {
	return "This turn is stopping. Wait for the process to exit before inspecting or resuming the job."
}

// SpawnFailed reports a provider that never launched — the one failure where
// an identical retry is provably safe.
func SpawnFailed(provider string, err error) string {
	return fmt.Sprintf("envoy could not start %s: %s", provider, err)
}

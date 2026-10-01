package prose

import (
	"fmt"
	"strings"

	"github.com/qiushiyan/envoy/internal/job"
)

// Recovery prescribes the next move from the prompt state alone — the only
// thing the engine can actually prove about a turn that did not return a
// result. resumeCmd continues the session, redispatchCmd re-sends the prompt
// as a new job; either is "" when the records cannot render it. The record's
// failure supplies any cause-specific remedy, and its launcher any configured
// command the caller must check before following that prescription.
//
// The three states carry different licenses, and confusing them is the
// expensive mistake: re-sending a prompt the provider already accepted
// duplicates work that may already have changed the tree.
func Recovery(m *job.Meta, resumeCmd, redispatchCmd string) string {
	fix := ""
	if remedy := remedy(m.Failure); remedy != "" {
		fix = " " + remedy
	}
	retryFailure := "the provider CLI itself is the problem to report."
	if m.UsesLauncher() {
		key := launcherEnv(m.Provider)
		if m.PromptState == job.PromptNotStarted {
			fix += fmt.Sprintf(" Fix %s or make its executable available first.", key)
		} else if m.PromptState != job.PromptAccepted {
			fix += fmt.Sprintf(" Check %s and stderr.log: the configured command may have stopped in the launcher or provider.", key)
		}
		fix += fmt.Sprintf(" Each follow-up reads the current value of %s.", key)
		retryFailure = "inspect the configured command and stderr.log."
	}
	switch m.PromptState {
	case job.PromptAccepted:
		if resumeCmd == "" {
			return "The provider accepted this prompt, so work may already exist in the working tree." + fix +
				" Read result.md, progress.log, and the tree to see how far it got before deciding anything else."
		}
		return "The provider accepted this prompt, so work may already exist in the session and the working tree." + fix +
			" Read result.md and check the tree, then continue the same session with a follow-up prompt:\n  " + resumeCmd +
			"\nSending the original prompt again would repeat work that already happened."

	case job.PromptNotStarted:
		body := "The provider never started, so this prompt did not run and nothing was changed." + fix
		if redispatchCmd == "" {
			return body + " Dispatch it once more under a new job name; if it fails the same way, " + retryFailure
		}
		return body + " Dispatch it once more under a new job name (a new name keeps this record collectable by name):\n  " + redispatchCmd +
			"\nIf it fails the same way, " + retryFailure

	default: // unknown
		body := "Whether the provider began work is unproven — silence is not proof that nothing ran." + fix +
			" Read progress.log, raw.log, stderr.log, and the working tree."
		if resumeCmd == "" {
			return body + " Re-dispatch under a new job name only once they show the prompt never began."
		}
		return body + " If any of them show work, continue the same session with a follow-up prompt:\n  " + resumeCmd +
			"\nRe-dispatch the original prompt under a new job name only once they show it never began."
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

// statusGloss says what a terminal status rules in or out, so the caller need
// not carry envoy's status table to act on one word.
func statusGloss(status string) string {
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
	if gloss := statusGloss(status); gloss != "" {
		return fmt.Sprintf("%s — %s", status, gloss)
	}
	return status
}

// DispatchNext is the nudge printed the moment a turn starts, when the caller
// is deciding whether to wait, poll, or walk away.
func DispatchNext(outDir string) string {
	return "let this command run to completion, then collect the job: " + CollectCommand(outDir)
}

// RunningNext is what a caller that collects too early should do instead.
func RunningNext(outDir string) string {
	return "This turn is still running. Wait for its process to exit, then collect it again: " + CollectCommand(outDir) + "."
}

// CollectedOK closes a successful collection by pointing at the payload.
func CollectedOK() string {
	return "result.md above is this turn's return value — use it in the step that dispatched it."
}

// StatusOnlyNext closes a status-only read, which delivers no result and
// therefore marks nothing collected.
func StatusOnlyNext(outDir string) string {
	return "this was a status check only — no result was printed and nothing was marked collected. " +
		"Print the full job: " + CollectCommand(outDir) + "."
}

// OkResultUnreadable closes a collection that found a terminal ok turn whose
// result.md could not be read: the answer was not delivered, so the job stays
// uncollected rather than being stamped over a payload nobody received.
func OkResultUnreadable(outDir string) string {
	return "this turn reports ok but its result.md could not be read, so its answer was not delivered and nothing " +
		"was marked collected. The payload may survive in raw.log or last-message.txt under " + outDir +
		"; recover it, then collect again: " + CollectCommand(outDir) + "."
}

// CollectThisJob is the action every terminal turn ends on.
func CollectThisJob(outDir string) string {
	return "Collect and verify this job: " + CollectCommand(outDir) + "."
}

// RunState classifies a turn still recorded as running by what its processes
// show. internal/collect decides which applies; the wording is here.
type RunState string

const (
	RunLive      RunState = "live"      // the runner is alive
	RunOrphaned  RunState = "orphaned"  // the runner is gone, a provider process is not
	RunAbandoned RunState = "abandoned" // the runner and the provider group are both gone
	RunUnknown   RunState = "unknown"   // the records cannot establish liveness
)

// RunObservation is what collect saw of a running turn's processes. State is
// "" for a turn that is not running.
type RunObservation struct {
	State RunState
	// RunnerPid is the recorded runner a live state saw alive; ProviderGroup
	// the recorded provider group — or, without one, the process — an orphan
	// left alive.
	RunnerPid     int
	ProviderGroup int
}

// RunDetail says what the processes of a running turn showed.
func RunDetail(o RunObservation) string {
	switch o.State {
	case RunLive:
		return fmt.Sprintf("a process with recorded runner PID %d is alive", o.RunnerPid)
	case RunOrphaned:
		return fmt.Sprintf("the runner is gone, but a process in the recorded provider group %d is still alive", o.ProviderGroup)
	case RunAbandoned:
		return "the recorded runner and provider process group are no longer alive"
	default:
		return "the job's process records are incomplete, so provider liveness cannot be established safely"
	}
}

// RunningStatus is the status line of a turn still recorded as running.
func RunningStatus(o RunObservation) string {
	return fmt.Sprintf("running (%s — %s; result.md is not final)", o.State, RunDetail(o))
}

// ---------- pending discovery ----------

// PendingUnreadable is why a job whose record will not read needs attention.
func PendingUnreadable(record string, err error) string {
	return fmt.Sprintf("%s could not be read: %s", record, err)
}

// PendingUnreadableNext is the one action for a job whose record will not
// read: the logs and the tree are the evidence left.
func PendingUnreadableNext(dir string) string {
	ws := job.Workspace{Dir: dir}
	return fmt.Sprintf("inspect %s, %s, %s, and the working tree; do not infer completion from the damaged metadata",
		ws.ProgressLogPath(), ws.RawLogPath(), ws.StderrLogPath())
}

// PendingOtherSchema is why an uncollected job written by another engine
// version is listed: its record — meta.json, or a fan-out's group.json — is
// intact, and this engine will not reinterpret it.
func PendingOtherSchema(record string, version, reads int) string {
	return fmt.Sprintf("its %s is schema %d, written by another envoy version; this one reads schema %d only and will not reinterpret it, so it cannot deliver the job", record, version, reads)
}

// PendingOtherSchemaNext is the one action for another version's job: its
// files are intact, and they are the evidence.
func PendingOtherSchemaNext(dir string) string {
	ws := job.Workspace{Dir: dir}
	return fmt.Sprintf("read %s and %s directly, or collect it with the envoy version that wrote it", ws.ResultPath(), ws.MetaPath())
}

// PendingOtherSchemaFanNext is the same action for another version's
// fan-out, whose results sit one level down, in its members' directories.
func PendingOtherSchemaFanNext(dir string) string {
	return fmt.Sprintf("read each member's result.md and meta.json under %s directly, or collect it with the envoy version that wrote it", dir)
}

// PendingUncollected is why a finished job needs attention.
func PendingUncollected(status string) string {
	return fmt.Sprintf("terminal status %s has not been collected", status)
}

// PendingMemberNoRecord is why a roster member with no record needs
// attention: the roster is the only evidence it was meant to run.
func PendingMemberNoRecord() string {
	return "has no meta.json, so it never recorded a start"
}

// PendingMembers rolls a fan-out's members up as one entry: one collect
// covers them all.
func PendingMembers(reasons []string, total int) string {
	return fmt.Sprintf("%d of %d members still need attention — %s", len(reasons), total, strings.Join(reasons, " · "))
}

// PendingNone closes an index with nothing in it.
func PendingNone() string {
	return "no recovery action is needed"
}

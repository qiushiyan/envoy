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

// Stopping is what to do while a stop is in flight.
func Stopping() string {
	return "This turn is stopping. Wait for the process to exit before inspecting or resuming the job."
}

// SpawnFailed reports a provider that never launched — the one failure where
// an identical retry is provably safe.
func SpawnFailed(provider string, err error) string {
	return fmt.Sprintf("envoy could not start %s: %s", provider, err)
}

// ---------- fan-out ----------
//
// A fan-out is several turns on one prompt, supervised as a single job. Its
// wording answers one extra question a single turn never raises: what one
// member's outcome licenses about another. The answer is nothing — members are
// independent turns, and each carries its own recovery — so every sentence
// here points the caller back to per-member actions instead of a group-wide
// retry.

// Fan-out aggregate statuses. Only "ok" is shared with a single turn; the
// others describe a set, and none of them replaces a member's own status.
const (
	FanRunning  = "running"
	FanOK       = "ok"
	FanPartial  = "partial"
	FanNoResult = "no-result"
)

// fanTally counts what a set of member statuses contains. A member that never
// reached a terminal status counts as one that returned no result.
func fanTally(statuses []string) (ok, running, total int) {
	for _, s := range statuses {
		switch s {
		case job.StatusOK:
			ok++
		case job.StatusRunning:
			running++
		}
	}
	return ok, running, len(statuses)
}

// FanStatus is the aggregate word for a set of member statuses.
func FanStatus(statuses []string) string {
	ok, running, total := fanTally(statuses)
	switch {
	case running > 0:
		return FanRunning
	case total > 0 && ok == total:
		return FanOK
	case ok > 0:
		return FanPartial
	default:
		return FanNoResult
	}
}

// FanStatusLine renders the aggregate with its gloss, so a caller acting on one
// word knows how many results exist and where the rest of the truth is.
func FanStatusLine(statuses []string) string {
	ok, running, total := fanTally(statuses)
	switch FanStatus(statuses) {
	case FanRunning:
		return fmt.Sprintf("running — %d of %s have not finished, so nothing here is final yet",
			running, turnCount(total))
	case FanOK:
		return fmt.Sprintf("ok — all %s returned a result", turnCount(total))
	case FanPartial:
		return fmt.Sprintf("partial — %d of %s returned a result; the rest each carry their own status and next action below",
			ok, turnCount(total))
	default:
		return fmt.Sprintf("no-result — none of the %s returned a result; each carries its own status and next action below",
			turnCount(total))
	}
}

func turnCount(n int) string {
	if n == 1 {
		return "1 turn"
	}
	return fmt.Sprintf("%d turns", n)
}

// FanUndispatched glosses a member that never reached a turn of its own: the
// one fan-out outcome where nothing ran and nothing was changed.
func FanUndispatched() string {
	return "never dispatched — envoy rejected or could not start this member, so nothing ran for it"
}

// FanDispatchNext is the nudge printed the moment a fan-out starts, when the
// caller is deciding how many things it now has to keep track of. The answer
// is one.
func FanDispatchNext(groupDir string) string {
	return "let this command run to completion — it exits once every member is done — then collect the fan-out once: " +
		CollectCommand(groupDir) +
		" · that single command returns every member's result, so there is nothing to track per member"
}

// FanStopping is what to do while a stop is in flight across every member.
func FanStopping(signalName string, total int) string {
	return fmt.Sprintf("received %s: stopping all %s. Wait for this process to exit, then collect the fan-out — "+
		"a member that had already finished still publishes its result.", signalName, turnCount(total))
}

// FanNext closes a finished fan-out. It prescribes collection and nothing
// else: what each member licenses depends on that member's prompt state, which
// its own section prints.
func FanNext(groupDir string, statuses []string) string {
	collect := "Collect the fan-out: " + CollectCommand(groupDir)
	switch FanStatus(statuses) {
	case FanRunning:
		return "This fan-out is still running. Wait for its process to exit, then collect it: " + CollectCommand(groupDir) + "."
	case FanOK:
		return collect + " — it prints every member's result in one block."
	case FanPartial:
		return collect + " — the members that returned a result are usable as they are, and each member that did not " +
			"carries its own next action in its section. One member's outcome licenses nothing about another: " +
			"re-dispatch or resume per member, never the whole fan-out."
	default:
		return collect + " — no member returned a result, and each member's section carries the one action to take for it. " +
			"Whether that member's prompt was accepted is what decides between a safe retry and duplicating work, " +
			"and the members can differ."
	}
}

// FanCollected closes a collection of the whole fan-out, pointing at the
// payload the way CollectedOK does for a single turn.
func FanCollected(groupDir string, statuses []string) string {
	switch FanStatus(statuses) {
	case FanRunning:
		return "This fan-out has members still running, so the sections above are not final. Wait for its process to exit, " +
			"then collect it again: " + CollectCommand(groupDir) + "."
	case FanOK:
		return "the member results above are this fan-out's return value — use them in the step that dispatched it."
	case FanPartial:
		return "the member results above are usable as they are. Each member that returned none carries its own next " +
			"action in its section — act on it per member; the members that succeeded need nothing."
	default:
		return "no member returned a result. Each section above carries the one action to take for that member, " +
			"and they can differ — a member whose prompt was never accepted is safe to re-run, one that was accepted is not."
	}
}

// FanAllowWriteRefused explains why a fan-out is read-only. Several turns
// editing one tree race each other, and the engine cannot make that safe.
func FanAllowWriteRefused() string {
	return "--allow-write is not available on a fan-out: its members share one working tree, and turns editing the " +
		"same files concurrently overwrite each other's work. For parallel write work, give each turn its own tree — " +
		"a git worktree per turn — and dispatch them as separate `envoy turn --allow-write --cwd <tree>` commands."
}

// FanResumeRefused explains why a bare session id cannot continue a fan-out,
// and where each of the two follow-up shapes lives.
func FanResumeRefused() string {
	return "--resume is not available on a fan-out: a session id names one conversation, and a fan-out runs several. " +
		"Resume one member with `envoy turn --resume <session>`, or continue every member of a finished fan-out on one " +
		"new prompt with `envoy fan --resume-from <fan-out-dir>` — `envoy collect` prints both commands."
}

// FanResumeFromAndWith explains why a resumed fan-out takes no member specs.
func FanResumeFromAndWith() string {
	return "--resume-from and --with are mutually exclusive: a resumed fan-out continues the members recorded in the " +
		"original fan-out's manifest, so the roster is already decided. Drop --with, or drop --resume-from to dispatch " +
		"a fresh fan-out."
}

// FanResumeFromNotAFanOut redirects a --resume-from aimed at a single turn.
// When that turn published a session, the redirect carries its actual resume
// command instead of a shape to imitate.
func FanResumeFromNotAFanOut(dir, resumeCmd string) string {
	head := fmt.Sprintf("--resume-from needs a fan-out directory, and %s is a single turn.", dir)
	if resumeCmd != "" {
		return head + " Continue it directly:\n  " + resumeCmd
	}
	return head + " Collect it to see what it licenses: " + CollectCommand(dir) + "."
}

// FanResumeBlockerKind classifies why one member blocks a set-level resume.
// The kinds are steer vocabulary so every surface that reports a blocker —
// the facade's refusal, collect's resume line — words one situation one way.
type FanResumeBlockerKind string

const (
	FanBlockerUnreadableMeta FanResumeBlockerKind = "unreadable-meta"
	FanBlockerRunning        FanResumeBlockerKind = "running"
	FanBlockerNoSession      FanResumeBlockerKind = "no-session"
	FanBlockerLockConflict   FanResumeBlockerKind = "lock-conflict"
)

// FanResumeBlockerLine words one member's blocker as an observation. detail
// carries the read error for an unreadable meta and is ignored otherwise.
func FanResumeBlockerLine(member string, kind FanResumeBlockerKind, detail string) string {
	switch kind {
	case FanBlockerUnreadableMeta:
		return fmt.Sprintf("member %s has no readable meta.json (%s)", member, detail)
	case FanBlockerRunning:
		return fmt.Sprintf("member %s is still running", member)
	case FanBlockerLockConflict:
		return fmt.Sprintf("member %s's last turn ended in a session-lock conflict, so its session may belong to another job", member)
	default: // FanBlockerNoSession
		return fmt.Sprintf("member %s never published a session id, so it has no conversation to continue", member)
	}
}

// FanResumeFromBlocked refuses a resume whose set is not whole: every member
// is continued together or not at all, because dispatching the ready ones now
// would leave the blocked voices out of the round with no record of why.
func FanResumeFromBlocked(dir string, reasons []string) string {
	return fmt.Sprintf("this fan-out cannot be resumed as a set yet — %s. A resumed fan-out continues every member "+
		"together. Collect it first (%s): that reconciles members that died without a status, and each section carries "+
		"its own recovery; once every member is finished with a session, resume the set — or continue just the ready "+
		"members individually with the `envoy turn --resume` command their sections print.",
		strings.Join(reasons, "; "), CollectCommand(dir))
}

// FanResumeSessionHeld refuses a round whose session reservation found a
// member's conversation held by another live turn. Nothing was dispatched:
// reservation happens before any member spawns, so the set stays whole.
func FanResumeSessionHeld(member, conflict string) string {
	return fmt.Sprintf("cannot start this round — member %s: %s\nA resumed fan-out starts every member or none, "+
		"and nothing was dispatched. Once that session is free, run this command again.",
		member, strings.TrimRight(conflict, ". \t\n")+".")
}

// FanResumeFromNoGroup rejects a --resume-from path that holds no fan-out.
func FanResumeFromNoGroup(dir string) string {
	return fmt.Sprintf("--resume-from %s: no fan-out found there (no group.json). Pass the fan-out's out-dir printed at dispatch", dir)
}

// FanResumeFromUnreadableManifest rejects a fan-out whose manifest cannot be
// parsed — nothing about its roster can be trusted, so nothing is dispatched.
func FanResumeFromUnreadableManifest(dir string, err error) string {
	return fmt.Sprintf("--resume-from %s: group.json is unreadable (%s)", dir, err)
}

// FanResumeFromEmptyManifest rejects a manifest that names no members.
func FanResumeFromEmptyManifest(dir string) string {
	return fmt.Sprintf("--resume-from %s: the manifest lists no members", dir)
}

// FanUndeliveredResults closes a group collection in which a member finished
// ok but its result body never reached the caller — the group must not claim
// every result above is usable when one of them was never printed.
func FanUndeliveredResults(members []string) string {
	noun := "member"
	if len(members) > 1 {
		noun = "members"
	}
	return fmt.Sprintf("not every result above was delivered: %s %s finished ok but the result body could not be read, "+
		"and that section carries the recovery. Delivered results are usable as they are; act on the rest per member.",
		noun, strings.Join(members, ", "))
}

// FanResumeCommand is the complete command that continues every member of this
// fan-out, mirroring Turn.ResumeCommand: settings carried, prompt file left as
// the placeholder a follow-up must fill.
func FanResumeCommand(dir string, timeoutMin float64) string {
	return fmt.Sprintf("envoy fan --resume-from %s --timeout-min %g --prompt-file <your-follow-up.md>",
		text.ShellQuote(dir), timeoutMin)
}

// FanMemberFlagRefused catches a turn flag aimed at a fan-out, where it would
// have to mean something different for each member.
func FanMemberFlagRefused(flag string) string {
	return fmt.Sprintf("%s is not available on a fan-out, where each member has its own. Put it in the member spec instead: "+
		"--with provider[:model[:effort]], for example --with codex --with claude:opus:high.", flag)
}

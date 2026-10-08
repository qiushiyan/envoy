package prose

import (
	"fmt"
	"strings"

	"github.com/qiushiyan/envoy/internal/job"
)

// A fan-out is several turns, each on its own prompt, supervised as a single
// job. Its wording answers one extra question a single turn never raises: what
// one member's outcome licenses about another. The answer is nothing — members
// are independent turns, and each carries its own recovery — so every sentence
// here points the caller back to per-member actions instead of a group-wide
// retry.

// Fan-out aggregate statuses. Only "ok" is shared with a single turn; the
// others describe a set, and none of them replaces a member's own status.
// FanOK alone is exported: whether every member returned a result is the one
// aggregate collect tests for.
const (
	fanRunning  = "running"
	FanOK       = "ok"
	fanPartial  = "partial"
	fanNoResult = "no-result"
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
		return fanRunning
	case total > 0 && ok == total:
		return FanOK
	case ok > 0:
		return fanPartial
	default:
		return fanNoResult
	}
}

// FanStatusLine renders the aggregate with its gloss, so a caller acting on one
// word knows how many results exist and where the rest of the truth is.
func FanStatusLine(statuses []string) string {
	ok, running, total := fanTally(statuses)
	switch FanStatus(statuses) {
	case fanRunning:
		return fmt.Sprintf("running — %d of %s have not finished, so nothing here is final yet",
			running, turnCount(total))
	case FanOK:
		return fmt.Sprintf("ok — all %s returned a result", turnCount(total))
	case fanPartial:
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
// FanMemberEnd is one member as a fan-out's ending block shows it: its
// directory and status, "" for a member that published none.
type FanMemberEnd struct {
	Name, Dir, Status string
}

// FanEnded is the block a fan-out dispatch prints once every member is done,
// and the one `envoy wait` prints for it later: the aggregate, each member's
// status and result, the manifest, and the one collect that covers them all.
func FanEnded(groupDir string, members []FanMemberEnd) string {
	statuses := make([]string, len(members))
	for i, m := range members {
		statuses[i] = m.Status
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nstatus: %s\n", FanStatusLine(statuses))
	for _, m := range members {
		if m.Status == "" {
			fmt.Fprintf(&b, "member %s: %s\n", m.Name, FanUndispatched())
			continue
		}
		fmt.Fprintf(&b, "member %s: %s · result %s\n", m.Name, StatusLine(m.Status), job.Workspace{Dir: m.Dir}.ResultPath())
	}
	fmt.Fprintf(&b, "group: %s\n", job.GroupWorkspace{Dir: groupDir}.GroupPath())
	fmt.Fprintf(&b, "next: %s\n", FanNext(groupDir, statuses))
	return b.String()
}

func FanUndispatched() string {
	return "never dispatched — envoy rejected or could not start this member, so nothing ran for it"
}

// FanMemberNoRecord is a fan-out member's section at collect time when its
// directory holds no meta.json. Unlike the supervisor at dispatch time, collect
// did not watch the runner return, so it says only what the directory shows
// and sends the reader to the logs before any retry.
func FanMemberNoRecord() string {
	return "no record — this member never wrote meta.json, so whether anything ran for it is unknown"
}

// FanMemberUnreadable is a fan-out member's section when its record is there
// and will not read — damaged, or written by another engine version. A member
// that wrote a record started, so it is never called undispatched; the
// section claims nothing past that.
func FanMemberUnreadable() string {
	return "unreadable — this member wrote meta.json, so it started, but the record could not be read (the reason is on stderr)"
}

// FanMemberUnreadableNext is the one action for a member whose record will
// not read: its files are the evidence, and this engine does not guess past
// a record it cannot read.
func FanMemberUnreadableNext(dir string) string {
	ws := job.Workspace{Dir: dir}
	return fmt.Sprintf("read %s and %s directly, with %s, %s and %s beside them, before deciding anything for this member",
		ws.ResultPath(), ws.MetaPath(), ws.ProgressLogPath(), ws.RawLogPath(), ws.StderrLogPath())
}

// FanMemberNoRecordNext is the one action for a member without a record.
func FanMemberNoRecordNext(dir string) string {
	ws := job.Workspace{Dir: dir}
	return fmt.Sprintf("read %s, %s and %s before retrying this member; a member whose logs show nothing ran is re-sent as its own job with its prompt.md",
		ws.ProgressLogPath(), ws.RawLogPath(), ws.StderrLogPath())
}

// FanDispatchNext is the nudge printed the moment a fan-out starts, when the
// caller is deciding how many things it now has to keep track of. The answer
// is one.
func FanDispatchNext(groupDir string) string {
	return "let this command run to completion — it exits once every member is done — then collect the job once: " +
		CollectCommand(groupDir) + " · that single command returns every member's result"
}

// FanStopping is what to do while a stop is in flight across every member.
func FanStopping(signalName string, total int) string {
	return fmt.Sprintf("received %s: stopping all %s. Wait for this process to exit, then collect the fan-out — "+
		"a member that had already finished still publishes its result.", signalName, turnCount(total))
}

// FanNext closes a finished fan-out. It prescribes collection and nothing
// else: what each member licenses depends on that member's prompt state, which
// its own section prints. Every member has returned by the time it prints, so
// none is running.
func FanNext(groupDir string, statuses []string) string {
	collect := "Collect the fan-out: " + CollectCommand(groupDir)
	switch FanStatus(statuses) {
	case FanOK:
		return collect + " — it prints every member's result in one block."
	case fanPartial:
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
	case fanRunning:
		return "This fan-out has members still running, so the sections above are not final. Wait for its process to exit, " +
			"then collect it again: " + CollectCommand(groupDir) + "."
	case FanOK:
		return "the member results above are this fan-out's return value — use them in the step that dispatched it."
	case fanPartial:
		return "the member results above are usable as they are. Each member that returned none carries its own next " +
			"action in its section — act on it per member; the members that succeeded need nothing."
	default:
		return "no member returned a result. Each section above carries the one action to take for that member, " +
			"and they can differ — a member whose prompt was never accepted is safe to re-run, one that was accepted is not."
	}
}

// FanResumeBlockerLine words one member's blocker as an observation, in the
// shared ResumeBlockerKind vocabulary. detail carries the read error for an
// unreadable meta and is ignored otherwise.
func FanResumeBlockerLine(member string, kind ResumeBlockerKind, detail string) string {
	switch kind {
	case BlockerUnreadableMeta:
		return fmt.Sprintf("member %s has no readable meta.json (%s)", member, detail)
	case BlockerRunning:
		return fmt.Sprintf("member %s is still running", member)
	case BlockerLockConflict:
		return fmt.Sprintf("member %s's last turn ended in a session-lock conflict, so its session may belong to another job", member)
	default: // BlockerNoSession
		return fmt.Sprintf("member %s never published a session id, so it has no conversation to continue", member)
	}
}

// FanContinueBlocked refuses a round whose set is not whole: every member
// is continued together or not at all, because dispatching the ready ones now
// would leave the blocked voices out of the round with no record of why.
func FanContinueBlocked(dir string, reasons []string) string {
	return fmt.Sprintf("--with @%s: this fan-out cannot be continued as a set yet — %s. A resumed fan-out continues every member "+
		"together. Collect it first (%s): that reconciles members that died without a status, and each section carries "+
		"its own recovery; once every member is finished with a session, continue the set — or continue just the ready "+
		"members individually with the command their sections print.",
		dir, strings.Join(reasons, "; "), CollectCommand(dir))
}

// FanContinueUnreadableManifest rejects a fan-out whose manifest cannot be
// parsed — nothing about its roster can be trusted, so nothing is dispatched.
func FanContinueUnreadableManifest(dir string, err error) string {
	return fmt.Sprintf("--with @%s: group.json is unreadable (%s)", dir, err)
}

// FanContinueEmptyManifest rejects a manifest that names no members.
func FanContinueEmptyManifest(dir string) string {
	return fmt.Sprintf("--with @%s: the manifest lists no members", dir)
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

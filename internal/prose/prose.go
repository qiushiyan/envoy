// Package prose holds every sentence envoy addresses to its caller.
//
// The caller is usually an AI agent reading stdout, so this text is a prompt
// surface: it must say what happened, what that rules in or out, and the one
// action to take next — and it may only prescribe what the engine actually
// observed. Centralizing it here keeps a single wording for each situation
// (the runner, the drivers, and collect all describe the same few outcomes)
// and makes the whole vocabulary reviewable in one file.
package prose

import (
	"fmt"
	"strings"
	"time"

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

// promptPlaceholder is the slot every follow-up command leaves open: a resumed
// turn needs a NEW prompt, and only the caller knows which file that will be.
// Commands are always rendered from structured fields — never edited as
// strings afterwards — so a path that happens to contain this text can never
// collide with the slot.
const promptPlaceholder = "<your-follow-up.md>"

// ResumeCommand is the complete command that continues this provider session,
// carrying the original turn's settings so a follow-up cannot silently drop
// write intent or run against another tree. The prompt file is a placeholder:
// a resumed turn needs a new prompt, never the original one again.
func (t Turn) ResumeCommand() string {
	return t.resumeCommand(promptPlaceholder)
}

// ResumeCommandWith renders the follow-up with an existing prompt file in the
// slot — steer's case, the one place the placeholder closes because the new
// prompt is already in the caller's hand.
func (t Turn) ResumeCommandWith(promptFile string) string {
	return t.resumeCommand(text.ShellQuote(promptFile))
}

func (t Turn) resumeCommand(promptArg string) string {
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
	parts = append(parts, fmt.Sprintf("--timeout-min %g", t.TimeoutMin), "--prompt-file "+promptArg)
	return strings.Join(parts, " ")
}

// CollectCommand prints one job as a single block; it is the caller's read
// path for every turn, healthy or not.
func CollectCommand(outDir string) string {
	return "envoy collect " + text.ShellQuote(outDir)
}

// CapStream is what the engine saw of the provider's stream at the moment a
// wall-clock cap ended the turn: how much arrived, and how long it had been
// quiet.
//
// It is an observation, never a verdict. The cap stays a deadline rather than
// a stall detector — envoy still never stops a turn for being quiet — but the
// caller deciding what to do next needs the difference between a turn that was
// mid-sentence when time ran out and one that went silent seconds after the
// prompt was accepted. Without it the caller can only read the cap, and a
// cap alone reads the same either way.
type CapStream struct {
	Events    int64         // provider events observed over the whole turn
	Bytes     int64         // raw bytes across the provider's stdout and stderr
	LastEvent string        // the last event's type; "" when none arrived
	Quiet     time.Duration // how long the stream had been silent when the cap fired
}

// ProviderStream words the tally of a provider's own connection-error
// events, as a diagnostic line beside the block — never inside the timeout
// gloss, because status and recovery follow from terminal and prompt evidence
// alone. It reports and stops: "connection-error events" is what the provider
// said, and "offline" would be the engine's inference. nil (an older engine
// never counted) prints nothing; zero is a real observation and prints.
func ProviderStream(ce *ConnectionErrors) string {
	if ce == nil {
		return ""
	}
	if ce.Count == 0 {
		return "provider stream: no connection-error events recognized by this engine version"
	}
	line := fmt.Sprintf("provider stream: %d recognized connection-error event%s", ce.Count, plural(ce.Count))
	if ce.FirstAt != "" && ce.LastAt != "" {
		line += fmt.Sprintf("; first observed %s, last observed %s", clockOf(ce.FirstAt), clockOf(ce.LastAt))
	}
	return line
}

// ConnectionErrors is the observation ProviderStream words; timestamps are
// RFC 3339 as meta.json records them.
type ConnectionErrors struct {
	Count   int64
	FirstAt string
	LastAt  string
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// clockOf renders an RFC 3339 instant as HH:MMZ, the resolution a caller
// comparing it to a cap or a commute needs; an unparseable stamp prints as is.
func clockOf(stamp string) string {
	if t, err := time.Parse(time.RFC3339, stamp); err == nil {
		return t.UTC().Format("15:04Z")
	}
	return stamp
}

// TimedOut is the terminal envelope of a turn the wall-clock cap ended: what
// happened, what reaching the cap does and does not prove, and what the stream
// itself looked like on the way there.
//
// The stream clause reports and stops. It never concludes that no work
// happened: bytes can arrive that yield no event envoy can parse, a provider
// can work for minutes between events, and work can exist in the session and
// the tree that never reached this process at all. Saying otherwise would
// contradict the recovery line printed beside it, which is the part that
// actually licenses the caller's next move.
func TimedOut(timeoutMin float64, provider string, stream CapStream) string {
	line := fmt.Sprintf(
		"The %g-minute wall-clock cap ended this %s turn. The cap counts healthy work too, so reaching it is not evidence the provider hung.",
		timeoutMin, provider)
	switch {
	case stream.Events > 0 && stream.LastEvent != "":
		return line + fmt.Sprintf(" When the cap arrived the stream had been quiet for %s, after %s (last: %s).",
			text.FormatDuration(stream.Quiet), eventCount(stream.Events), stream.LastEvent)
	case stream.Bytes > 0:
		return line + fmt.Sprintf(" The provider wrote %d bytes before the cap but no event envoy could parse, so nothing here records what it was doing.",
			stream.Bytes)
	default:
		return line + " The provider wrote nothing at all before the cap — no output on either stream, and no events."
	}
}

func eventCount(n int64) string {
	if n == 1 {
		return "1 event"
	}
	return fmt.Sprintf("%d events", n)
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

// ---------- the job roster ----------
//
// The out-dir printed at dispatch stays the retained coordinate. The roster is
// what answers a caller that no longer has it — before it existed, callers
// rebuilt job paths by hand from the stamp-and-label convention, and the stamp
// is the dispatch second nobody knows. It hands back coordinates and nothing
// else: which job to read, and whether to read it at all, stays the caller's
// judgment.

// JobsHeader opens the roster: how many jobs it names, of how many the project
// holds, and where they live.
func JobsHeader(shown, total int, base string) string {
	switch {
	case total == 0:
		return fmt.Sprintf("jobs: 0 (under %s)", base)
	case shown < total:
		return fmt.Sprintf("jobs: %d of %d (under %s, newest first)", shown, total, base)
	}
	return fmt.Sprintf("jobs: %d (under %s, newest first)", total, base)
}

// JobsNext closes the roster. A listing delivers no result, so — like
// --status-only — it marks nothing collected and every job it names stays owed.
// truncated adds the way to reach the jobs this listing left out, so an old
// coordinate never becomes unrecoverable.
func JobsNext(truncated bool) string {
	line := "this is a listing only — no result was printed and nothing was marked collected. " +
		"Print one job in full by passing its dir above: envoy collect <job-dir>"
	if truncated {
		line += " · older jobs than these: envoy jobs --all"
	}
	return line
}

// JobsNone answers a project that has never dispatched a turn, which is not a
// lost coordinate but an empty store.
func JobsNone() string {
	return "no turn has run in this project yet — dispatch one with envoy turn or envoy fan"
}

// UnreadableStore refuses to report an unreadable job root as an empty one.
// The two look identical from the outside and mean opposite things: a mistyped
// --base and a project that has never dispatched both print zero jobs, and
// only one of them is a fact the engine observed.
func UnreadableStore(base string, err error) string {
	return fmt.Sprintf("the job store at %s could not be read: %s. "+
		"This is not the same as an empty store, so nothing here says whether that project has jobs — "+
		"check the path and its permissions.", base, err)
}

// ---------- continuing a finished job ----------
//
// --resume-from (a whole turn, or a whole fan-out) and --with-from (one
// finished job's session as a member of a new fan-out) anchor a follow-up on
// a job's own records instead of a hand-carried session id. The records are
// the safer coordinate: a resumed claude conversation continues under a fresh
// id, so the id a caller remembers goes stale while the job dir's meta always
// names the current one. The wording below covers the two questions that
// surface raises: which jobs can be continued, and which command fits the
// shape at hand.

// ResumeBlockerKind classifies why a job's session cannot be continued. The
// kinds are prose vocabulary shared by every surface that reports one — the
// fan facade's refusal, collect's resume line, and the --resume-from and
// --with-from refusals — so one situation is worded one way everywhere.
type ResumeBlockerKind string

const (
	BlockerUnreadableMeta ResumeBlockerKind = "unreadable-meta"
	BlockerRunning        ResumeBlockerKind = "running"
	BlockerNoSession      ResumeBlockerKind = "no-session"
	BlockerLockConflict   ResumeBlockerKind = "lock-conflict"
)

// TurnResumeFromCommand is the complete command that continues a finished
// turn's session by naming its job dir, settings read from that job's own
// records. The prompt file stays the placeholder: a resumed turn needs a NEW
// prompt.
func TurnResumeFromCommand(dir string) string {
	return "envoy turn --resume-from " + text.ShellQuote(dir) + " --prompt-file " + promptPlaceholder
}

// ResumeFromAndResume explains why the two ways of naming a conversation
// cannot combine on one turn.
func ResumeFromAndResume() string {
	return "--resume-from and --resume are mutually exclusive: both name the conversation to continue. " +
		"Point --resume-from at the job dir to let its records supply the session and settings, " +
		"or use --resume <session-id> and spell the settings yourself."
}

// ResumeFromAndProvider explains why --resume-from takes no provider flag.
func ResumeFromAndProvider() string {
	return "--provider is already decided by --resume-from: the job's records name it, and a conversation " +
		"cannot move to another provider. Drop --provider; --model and --effort still override how the follow-up runs."
}

// ResumeFromNoTurn rejects a --resume-from/--with-from path that holds no
// readable turn. cause is the observation ("no turn found there (no
// meta.json)", a parse error); the prescription is the same for all of them.
func ResumeFromNoTurn(flag, dir string, cause error) string {
	return fmt.Sprintf("%s %s: %s. Pass the out-dir printed when the job was dispatched — or a fan-out member's directory.",
		flag, dir, cause)
}

// FanMemberCandidate is one member of the fan-out a job-dir flag was aimed
// at: the coordinate the caller can name instead of the group, or the
// observation that blocks it (Blocked is "" when the member may continue).
type FanMemberCandidate struct {
	Name    string
	Dir     string
	Blocked string
}

// ResumeFromIsFanOut redirects a job-dir flag aimed at a fan-out directory: a
// group holds several conversations, so the caller names one member or — on
// a round — the set. The two flags want different next actions, so each gets
// its own: --with-from continues one member beside cold ones and is handed
// copy-ready member flags, never the whole-set round (that continues every
// member and starts nobody cold); --resume-from on a turn is handed the
// round first and the per-member turn commands after. Only members the
// shared eligibility check clears are offered as commands; the rest print
// their blocker, so nothing here advertises what dispatch would refuse.
// roundCmd is the runnable set-level round, "" when the manifest is unreadable.
func ResumeFromIsFanOut(flag, dir string, members []FanMemberCandidate, roundCmd string) string {
	head := fmt.Sprintf("%s %s: this is a fan-out, and its members hold their own sessions.", flag, dir)
	var offered, blocked []string
	for _, m := range members {
		if m.Blocked != "" {
			blocked = append(blocked, "  "+m.Blocked)
			continue
		}
		if flag == "--with-from" {
			offered = append(offered, fmt.Sprintf("  --with-from %s  (%s)", text.ShellQuote(m.Dir), m.Name))
		} else {
			offered = append(offered, fmt.Sprintf("  %s  (%s)", TurnResumeFromCommand(m.Dir), m.Name))
		}
	}
	var b strings.Builder
	b.WriteString(head)
	if flag == "--with-from" {
		if len(offered) > 0 {
			b.WriteString(" Continue one member's session beside cold members by naming its directory:\n")
			b.WriteString(strings.Join(offered, "\n"))
		} else {
			b.WriteString(" Name one member's directory to continue that session beside cold members.")
		}
		if len(blocked) > 0 {
			b.WriteString("\nNot continuable:\n" + strings.Join(blocked, "\n"))
		}
		if roundCmd != "" {
			b.WriteString("\n(To continue every member as one round, with no cold member: " + roundCmd + ")")
		}
		return b.String()
	}
	if roundCmd != "" {
		b.WriteString(" Continue every member as one round:\n  " + roundCmd)
	}
	if len(offered) > 0 {
		b.WriteString("\nOr continue one member's directory alone:\n" + strings.Join(offered, "\n"))
	} else if roundCmd == "" {
		b.WriteString(" Name one member's directory to continue that voice alone, or continue every member as one round with envoy fan --resume-from.")
	}
	if len(blocked) > 0 {
		b.WriteString("\nNot continuable:\n" + strings.Join(blocked, "\n"))
	}
	return b.String()
}

// ResumeFromBlocked refuses to continue a job whose session is not provably
// free to continue, naming the observation and the one safe next step.
func ResumeFromBlocked(flag, dir string, kind ResumeBlockerKind) string {
	collect := CollectCommand(dir)
	head := fmt.Sprintf("%s %s: ", flag, dir)
	switch kind {
	case BlockerRunning:
		return head + "this job still records status running, and a session admits one live turn at a time. " +
			"If it is genuinely running, wait for its process to exit; if you believe it died, collect it first — " +
			collect + " — and follow its next line."
	case BlockerLockConflict:
		return head + "this job's last turn ended in a session-lock conflict, so its session may belong to another job. " +
			"Collect it first — " + collect + " — and follow its next line before continuing this conversation."
	default: // BlockerNoSession
		return head + "this job never published a session id, so it holds no conversation to continue. " +
			"Collect it — " + collect + " — to see what recovery it licenses; a fresh dispatch may be the right move."
	}
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
// and where each of the follow-up shapes lives.
func FanResumeRefused() string {
	return "--resume is not available on a fan-out: a session id names one conversation, and a fan-out runs several. " +
		"Resume one member with `envoy turn --resume <session>`, continue every member of a finished fan-out on one " +
		"new prompt with `envoy fan --resume-from <fan-out-dir>` — `envoy collect` prints both commands — or continue " +
		"one finished job's session as a member of a new roster with `--with-from <its-job-dir>`."
}

// FanResumeFromAndWith explains why a resumed fan-out takes no member specs.
func FanResumeFromAndWith() string {
	return "--resume-from and --with are mutually exclusive: a resumed fan-out continues the members recorded in the " +
		"original fan-out's manifest, so the roster is already decided. Drop --with, or drop --resume-from to dispatch " +
		"a fresh fan-out."
}

// FanResumeFromAndWithFrom keeps the two continuation shapes apart: the whole
// original set, or a new roster built member by member.
func FanResumeFromAndWithFrom() string {
	return "--resume-from and --with-from are mutually exclusive: a resumed fan-out already continues every member of " +
		"the original, so there is no roster to build. Use --resume-from alone to continue the whole set, or compose a " +
		"new roster from --with-from and --with members."
}

// FanSingleWithFrom redirects a one-member fan-out to the single-turn form,
// with the caller's actual dir already in the command.
func FanSingleWithFrom(dir string) string {
	return "a fan-out needs at least two members. To continue this one session by itself, use:\n  " +
		TurnResumeFromCommand(dir) +
		"\nOr add more voices: another --with-from <job-dir>, or a fresh --with provider[:model[:effort]]."
}

// FanWithFromDuplicateSession refuses a roster that names one conversation
// twice: a session admits a single live turn, so the second member could
// never dispatch.
func FanWithFromDuplicateSession(session, firstDir, secondDir string) string {
	return fmt.Sprintf("--with-from names the same conversation twice (session %s, from %s and %s), and a session "+
		"admits one live turn at a time. Name each conversation once; add fresh voices with --with.",
		session, firstDir, secondDir)
}

// FanWithFromCwdMix asks for an explicit tree when the continued jobs do not
// share one: a fan-out's members run in a single working directory, and the
// engine will not choose between the recorded ones.
func FanWithFromCwdMix(firstDir, firstCwd, secondDir, secondCwd string) string {
	return fmt.Sprintf("the jobs named by --with-from ran in different working directories (%s ran in %s; %s ran in %s). "+
		"A fan-out's members share one tree — pass --cwd to choose it.",
		firstDir, firstCwd, secondDir, secondCwd)
}

// FanWithFromBaselineMix asks for an explicit anchor when the continued jobs
// recorded different ones: a fan-out reports one reviewed range, and the
// engine will not choose which conversation's anchor wins.
func FanWithFromBaselineMix(firstDir, firstBaseline, secondDir, secondBaseline string) string {
	return fmt.Sprintf("the jobs named by --with-from recorded different baselines (%s recorded %s; %s recorded %s). "+
		"A fan-out reports one reviewed range — pass --baseline to choose the anchor.",
		firstDir, firstBaseline, secondDir, secondBaseline)
}

// FanWithFromWriteSource refuses to continue a write conversation inside a
// fan-out, whose members are read-only: silently narrowing the conversation's
// write intent is the exact drop the inheritance contract forbids, so the
// intent is kept by continuing the conversation alone.
func FanWithFromWriteSource(dir string) string {
	return fmt.Sprintf("--with-from %s: that job ran with --allow-write, and a fan-out's members are read-only — "+
		"they share one working tree. Continue this conversation alone, keeping its write intent:\n  %s",
		dir, TurnResumeFromCommand(dir))
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
	return fanResumeCommand(dir, timeoutMin, promptPlaceholder)
}

// FanResumeCommandWith renders the round with an existing prompt file in the
// slot, mirroring Turn.ResumeCommandWith.
func FanResumeCommandWith(dir string, timeoutMin float64, promptFile string) string {
	return fanResumeCommand(dir, timeoutMin, text.ShellQuote(promptFile))
}

func fanResumeCommand(dir string, timeoutMin float64, promptArg string) string {
	return fmt.Sprintf("envoy fan --resume-from %s --timeout-min %g --prompt-file %s",
		text.ShellQuote(dir), timeoutMin, promptArg)
}

// FanMemberFlagRefused catches a turn flag aimed at a fan-out, where it would
// have to mean something different for each member.
func FanMemberFlagRefused(flag string) string {
	return fmt.Sprintf("%s is not available on a fan-out, where each member has its own. Put it in the member spec instead: "+
		"--with provider[:model[:effort]], for example --with codex --with claude:opus:high.", flag)
}

// ---------- steer ----------
//
// `envoy steer` answers one question about a dispatched job: can a
// supplemental prompt still reach it? The answer is always no — no provider
// accepts input into a running turn (claude's streaming input would queue it
// as a separate turn; codex exec reads its instructions once, at dispatch) —
// so every report below opens with "not delivered" and hands over the one
// command that does carry the supplement: the follow-up turn that continues
// the same session, with the supplement already in its --prompt-file slot.
// The engine delivers nothing, mutates nothing, and stamps nothing collected.

// SteerReport is the whole answer to one steer request: why the supplement
// was not delivered, and the one action that carries it forward.
type SteerReport struct {
	Why  string // one clause, printed after "not delivered — "
	Next string // the action, possibly spanning lines with indented commands
}

// Block renders the complete steer answer, so the "not delivered" verdict —
// the one fact every steer report shares — is worded here with the rest of
// the vocabulary rather than at the printing surface.
func (r SteerReport) Block(outDir string) string {
	return "steer: not delivered — " + r.Why + "\n" +
		"job: " + outDir + "\n" +
		"next: " + r.Next + "\n"
}

// SteerCommand is the steer invocation for one job dir, spelled out so a
// fan-out's members can each be handed their own runnable line.
func SteerCommand(promptFile, outDir string) string {
	return "envoy steer --prompt-file " + text.ShellQuote(promptFile) + " " + text.ShellQuote(outDir)
}

// SteerLive reports a genuinely live turn. resumeCmd is the follow-up command
// with the supplement already filled in, or "" when the turn has not published
// a session id yet (a fresh codex thread before thread.started).
func SteerLive(provider, resumeCmd, outDir string) SteerReport {
	var why string
	switch provider {
	case "claude":
		why = "this turn is still running, and claude takes no input into a turn in flight — " +
			"its streaming input would only queue the supplement as a second turn after this one finishes"
	case "codex":
		why = "this turn is still running, and codex takes no input after dispatch — " +
			"codex exec reads its instructions once, at start"
	default:
		why = "this turn is still running, and no provider accepts input into a turn in flight"
	}
	if resumeCmd == "" {
		return SteerReport{Why: why, Next: "no session id has been published yet, so the follow-up command " +
			"cannot be printed here. Wait for this job to finish, then collect it (" + CollectCommand(outDir) +
			") and give this supplement to the resume command it prints."}
	}
	return SteerReport{Why: why, Next: "wait for this job to finish and read its result — it may already cover " +
		"this — then send the supplement as the same session's follow-up prompt:\n  " + resumeCmd}
}

// SteerStale reports a job whose meta says running while its runner is gone.
// Steer prescribes nothing itself: collect owns the stranded-turn diagnosis,
// and a second reader would drift from it.
func SteerStale(kind, outDir string) SteerReport {
	return SteerReport{
		Why: "this job records status running but its runner process is gone (" + kind + "), so nothing is listening for input",
		Next: "collect the job: " + CollectCommand(outDir) + " — that diagnoses the stranded turn and prescribes " +
			"the safe continuation; give this supplement to whatever follow-up command it prints.",
	}
}

// SteerTerminalOK reports a finished ok turn: the supplement is simply the
// session's next prompt. An uncollected result is read first — it may already
// cover what the supplement asks.
func SteerTerminalOK(resumeCmd, outDir string, collected bool) SteerReport {
	why := "this job is already terminal (status ok), so there is no running turn to reach"
	if !collected {
		return SteerReport{Why: why, Next: "read the result first — it may already cover this: " + CollectCommand(outDir) +
			". Then send the supplement as the same session's follow-up prompt:\n  " + resumeCmd}
	}
	return SteerReport{Why: why, Next: "send the supplement as the same session's follow-up prompt:\n  " + resumeCmd}
}

// SteerTerminalNotOK reports a finished non-ok turn without prescribing a
// resume: whether that session may continue is the job's own recovery
// decision, which follows from its prompt state and is printed by collect.
func SteerTerminalNotOK(status, outDir string) SteerReport {
	return SteerReport{
		Why: "this job is already terminal (status " + status + "), so there is no running turn to reach",
		Next: "whether its session may continue is that job's own recovery decision. Collect it and follow its " +
			"next line, giving this supplement to any follow-up command it prints: " + CollectCommand(outDir),
	}
}

// SteerGroup redirects a steer aimed at a fan-out directory: members hold
// independent sessions, so the supplement goes to one member — or to all of
// them as a new round once the set is finished.
func SteerGroup(memberCmds []string, fanResumeCmd string) SteerReport {
	return SteerReport{
		Why: "this is a fan-out, and its members hold independent sessions that can be in different states",
		Next: "steer one member:\n  " + strings.Join(memberCmds, "\n  ") +
			"\nOr send the supplement to every member as one new round once the fan-out is finished:\n  " + fanResumeCmd,
	}
}

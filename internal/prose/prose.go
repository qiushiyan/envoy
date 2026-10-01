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

// Voice spells a member the way a caller types it: provider[:model[:effort]].
// An effort without a model keeps the middle slot empty (codex::high).
func Voice(provider, model, effort string) string {
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
	voice := Voice(m.Provider, job.Deref(m.Model), job.Deref(m.Effort))
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
// said, and "offline" would be the engine's inference. nil (the driver does
// not observe them, or an older engine never counted) prints nothing; zero is
// a real observation and prints.
func ProviderStream(ce *job.ConnectionErrors) string {
	if ce == nil {
		return ""
	}
	if ce.Count == 0 {
		return "provider stream: no connection-error events recognized by this engine version"
	}
	line := fmt.Sprintf("provider stream: %d recognized connection-error event%s", ce.Count, plural(ce.Count))
	if ce.FirstAt != nil && ce.LastAt != nil {
		line += fmt.Sprintf("; first observed %s, last observed %s", clockOf(*ce.FirstAt), clockOf(*ce.LastAt))
	}
	return line
}

// ContextUsage describes the latest sampled request input. It belongs in the
// diagnostic tier; incompleteness is telemetry, never a recovery prescription.
func ContextUsage(u *job.Usage) string {
	if u == nil {
		return ""
	}
	line := "context: no measurement yet"
	if u.LatestContextTokens != nil {
		line = "context: " + compactTokens(*u.LatestContextTokens)
		if u.ContextWindowTokens != nil && *u.ContextWindowTokens > 0 {
			line += fmt.Sprintf(" of %s (%.0f%%)", compactTokens(*u.ContextWindowTokens),
				100*float64(*u.LatestContextTokens)/float64(*u.ContextWindowTokens))
		} else {
			line += ", window unknown"
		}
		if u.PeakContextTokens != nil {
			line += ", peak " + compactTokens(*u.PeakContextTokens)
		}
		line += fmt.Sprintf(", %d response%s", u.Responses, plural(u.Responses))
	}
	var issues []string
	for _, issue := range u.Issues {
		var detail string
		switch issue {
		case "missing_terminal":
			detail = "no terminal record"
		case "missing_window":
			detail = "window unknown"
		case "missing_context_sample":
			detail = "no context sample"
		case "missing_model":
			detail = "model attribution missing"
		case "model_mismatch":
			detail = "unexpected model"
			if u.UnexpectedModel != nil {
				detail += fmt.Sprintf(" %q", *u.UnexpectedModel)
			}
		case "missing_message_id":
			detail = "response ID missing"
		case "missing_input_usage":
			detail = "response input usage missing or invalid"
		case "missing_terminal_usage":
			detail = "terminal usage missing or invalid"
		case "terminal_usage_incomplete":
			detail = "terminal usage incomplete"
		case "terminal_usage_mismatch":
			detail = "terminal usage disagrees with observed responses"
		default:
			detail = "measurement evidence incomplete"
		}
		issues = append(issues, detail)
	}
	if len(issues) > 0 {
		line += "; incomplete (" + strings.Join(issues, "; ") + ")"
	}
	return line
}

func compactTokens(n int64) string {
	unit, suffix := float64(1), ""
	switch {
	case n >= 1000000:
		unit, suffix = 1000000, "M"
	case n >= 1000:
		unit, suffix = 1000, "k"
	default:
		return fmt.Sprint(n)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/unit), ".0") + suffix
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
// result. resumeCmd continues the session, redispatchCmd re-sends the prompt
// as a new job; either is "" when the records cannot render it. The record
// supplies the driver's cause-specific remedy and any configured command
// the caller must check before following that prescription.
//
// The three states carry different licenses, and confusing them is the
// expensive mistake: re-sending a prompt the provider already accepted
// duplicates work that may already have changed the tree.
func Recovery(m *job.Meta, resumeCmd, redispatchCmd string) string {
	fix := ""
	if remedy := job.Deref(m.Remedy); remedy != "" {
		fix = " " + strings.TrimSpace(remedy)
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

// SpawnFailed reports a command that never launched — the one failure where
// re-sending the same prompt is provably safe.
func SpawnFailed(command string, err error) string {
	return fmt.Sprintf("envoy could not start %s: %s", command, err)
}

func launcherEnv(provider string) string { return "ENVOY_" + strings.ToUpper(provider) + "_CMD" }

// Launcher renders the recorded invocation, never the collector's environment.
func Launcher(m *job.Meta) string {
	if !m.UsesLauncher() {
		return ""
	}
	return fmt.Sprintf("launcher: %s=%s", launcherEnv(m.Provider), text.ShellQuote(strings.Join(m.CommandPrefix, " ")))
}

// Setting renders a model or effort as the turn was dispatched with it. An
// omitted one is the provider's own configuration, reported as such — never a
// guess at what that configuration resolves to.
func Setting(v string) string {
	if v == "" {
		return "(provider default)"
	}
	return v
}

// ModelSetting renders the requested model with the one the provider reported
// running, when that report spells something the request did not. A
// difference is the provider's own alias resolution as often as anything
// else, so this shows both and concludes nothing.
func ModelSetting(requested, reported string) string {
	switch {
	case reported == "" || reported == requested:
		return Setting(requested)
	case requested == "":
		return fmt.Sprintf("(provider default, ran %s)", reported)
	default:
		return fmt.Sprintf("%s (ran %s)", requested, reported)
	}
}

// ProcessExited distinguishes an observed command exit from a provider verdict.
// A launcher can exit before the provider runs or remain as its parent.
func ProcessExited(provider, command string, code *int, observation, detail string) string {
	name, source := provider, "provider"
	if command != "" {
		name, source = fmt.Sprintf("Command %q", command), "command"
	}
	codeText := "null"
	if code != nil {
		codeText = fmt.Sprintf("%d", *code)
	}
	line := fmt.Sprintf("%s exited with code %s %s.", name, codeText, observation)
	if detail != "" {
		line += fmt.Sprintf(" Last %s detail: %s", source, detail)
	}
	return line
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

// NameFellBack is the note on a block whose name did not resolve to a job of
// the caller's own: the caller has an identity and no job under the name, so
// it read the newest of anyone's. That is how earlier work is picked up, and
// also what a caller whose identity changed mid-task gets in place of its
// own job — the engine cannot tell the two apart, so it says which happened
// and leaves the judgment to the caller.
func NameFellBack(name string, ownerKnown bool) string {
	owner := "a caller with no recorded session dispatched it"
	if ownerKnown {
		owner = "another session dispatched it"
	}
	return fmt.Sprintf("this session has dispatched no job named %s, so this is the newest job under that name — %s. "+
		"Check it is the job you mean before using it: a job's prompt.md holds what it was asked.", name, owner)
}

// ContinuedAnotherSession is the note on a turn whose conversation was begun
// by a different session than the one that dispatched this turn.
func ContinuedAnotherSession() string {
	return "this turn continued a conversation another session began (resumed-from, above). " +
		"If you meant to continue your own, this result does not belong to it: send the follow-up again, naming your own job by its directory."
}

// UnattributedGeneration stops a name from resolving past a job whose record
// cannot say whose it is. Skipping it would let an older job of the caller's
// answer to the name as though it were the latest, which is the wrong-job
// read names exist to prevent.
func UnattributedGeneration(name, dir string, err error) string {
	return fmt.Sprintf("the name %s was not resolved: the record in %s could not be read (%s), so the name cannot be shown to mean this session's latest job. "+
		"Name the job you want by its directory; to see what that one holds: %s", name, dir, err, CollectCommand(dir))
}

// ---------- continuing a finished job ----------
//
// A voice spelled @<job> continues that job's conversation as a member of
// the new job, settings read from its records instead of a hand-carried
// session id. The records are the safer coordinate: a resumed claude
// conversation continues under a fresh id, so the id a caller remembers goes
// stale while the job dir's meta always names the current one. A fan-out
// reference continues every member at once and therefore stands alone.

// ResumeBlockerKind classifies why a job's session cannot be continued. The
// kinds are prose vocabulary shared by every surface that reports one — the
// dispatch refusal and collect's resume line — so one situation is worded
// one way everywhere.
type ResumeBlockerKind string

const (
	BlockerUnreadableMeta ResumeBlockerKind = "unreadable-meta"
	BlockerRunning        ResumeBlockerKind = "running"
	BlockerNoSession      ResumeBlockerKind = "no-session"
	BlockerLockConflict   ResumeBlockerKind = "lock-conflict"
)

// ContinueNoTurn rejects an @<job> that holds no readable job. cause is the
// observation ("no job found there (no meta.json)", a parse error).
func ContinueNoTurn(dir string, cause error) string {
	return fmt.Sprintf("--with @%s: %s. Name a finished job — by the name it was run as, or by its directory.", dir, cause)
}

// FanMemberCandidate is one member of a fan-out named beside other voices:
// the coordinate the caller can name instead of the group, with the
// eligibility observation still structured so rendering happens here, once.
type FanMemberCandidate struct {
	Name    string
	Dir     string
	Blocked bool
	Kind    ResumeBlockerKind
	Detail  string
}

// GroupRefMustStandAlone refuses @<fan-out> mixed with other voices: a
// fan-out reference continues every member as one round, so beside other
// voices it would silently turn "continue the round" into roster
// composition. The eligible members are offered as the voices to name.
func GroupRefMustStandAlone(dir string, members []FanMemberCandidate) string {
	var offered, blocked []string
	for _, m := range members {
		if m.Blocked {
			blocked = append(blocked, "  "+FanResumeBlockerLine(m.Name, m.Kind, m.Detail))
			continue
		}
		offered = append(offered, fmt.Sprintf("  --with @%s  (%s)", text.ShellQuote(m.Dir), m.Name))
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("--with @%s names a fan-out, which continues every member as one round and so stands alone as the only --with.", dir))
	if len(offered) > 0 {
		b.WriteString(" To seat one member's conversation beside other voices, name that member:\n" + strings.Join(offered, "\n"))
	}
	if len(blocked) > 0 {
		b.WriteString("\nNot continuable:\n" + strings.Join(blocked, "\n"))
	}
	return b.String()
}

// ContinueBlocked refuses to continue a job whose session is not provably
// free to continue, naming the observation and the one safe next step.
func ContinueBlocked(dir string, kind ResumeBlockerKind) string {
	collect := CollectCommand(dir)
	head := fmt.Sprintf("--with @%s: ", dir)
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

// ---------- naming and rostering a run ----------

// JobExists refuses a directory path that already exists. A path is an
// identity — one directory, one job — so unlike a name it has no generations.
func JobExists(dir string) string {
	return fmt.Sprintf("job directory %s already exists — a directory holds one job. Pick another path; "+
		"to read the existing job: %s", dir, CollectCommand(dir))
}

// NameHoldKind classifies why a job still holds its name. The vocabulary is
// owned here; internal/collect decides which kind applies.
type NameHoldKind string

const (
	HoldRunning     NameHoldKind = "running"
	HoldUncollected NameHoldKind = "uncollected"
	HoldUnrecorded  NameHoldKind = "unrecorded"
	HoldUnreadable  NameHoldKind = "unreadable"
)

// NameHold is what internal/collect observed holding a name: why, through
// which fan-out member if any, and — for a directory with no record —
// whether it was seen to be empty.
type NameHold struct {
	Kind   NameHoldKind
	Member string
	Empty  bool
}

// NameHeld refuses a dispatch under a name held by an undelivered job that
// shares the dispatcher's meaning of it. Every other reuse of a name is
// silent, so this is the one collision a caller ever hears about, and it leads with the fact a caller must not
// miss: nothing ran, so collecting this name reads the job already there —
// the caller's own earlier one, or another caller's. dir is the job — for a
// fan-out the whole one, since one collect covers every member.
func NameHeld(name, dir string, hold NameHold) string {
	subject := "which"
	if hold.Member != "" {
		subject = fmt.Sprintf("whose member %s", hold.Member)
	}
	// What a collect of the name would do differs by holder, and only the
	// two kinds with a finished-or-running job behind them can be read at all:
	// the caller then gets a job that was already there — the holder, or an
	// older one of its own — so the sentence claims no more than "earlier".
	earlier := fmt.Sprintf(" Collecting %s now reads an earlier job, not a new one.", name)
	var state, release string
	switch hold.Kind {
	case HoldRunning:
		state = "is still running"
		release = earlier + " The name frees once that job has finished and its own caller has collected it."
	case HoldUncollected:
		state = "finished but has not been collected"
		release = earlier + " If that job is yours or its caller is gone, collecting it frees the name: " + CollectCommand(dir)
	case HoldUnrecorded:
		switch {
		case hold.Member != "":
			state = "has written no record while the process running that fan-out may still be alive, so it may yet start"
			release = earlier + " The name frees once that fan-out has finished and its own caller has collected it."
		case hold.Empty:
			state = "was reserved and holds no record — a dispatch in its first moments, or one that died there, and nothing here says which"
			release = " Once you have established that no envoy run owns that directory and it is still empty, removing it frees the name."
		default:
			state = "was reserved and holds no record — a dispatch in its first moments, or one that died there, and nothing here says which"
			release = " Read what that directory holds, and establish that no envoy run owns it, before anything reuses its name."
		}
	default:
		state = "holds a record that cannot be read"
		release = " Inspect that directory before anything reuses its name."
	}
	return fmt.Sprintf("nothing was dispatched: the name %s is held by another job, %s, %s %s. "+
		"Dispatch again under a different name (%s-b, say).%s",
		name, dir, subject, state, name, release)
}

// RunNeedsVoice refuses a run with no member at all.
// VoiceNeedsPrompt is the refusal for a voice that has no prompt from either
// source: its own seat (--with voice=file) or the job's default (--prompt-file).
func VoiceNeedsPrompt(spec string) string {
	return fmt.Sprintf("--with %s has no prompt: attach one to the voice (--with %s=<file>) or give every voice a default with --prompt-file <file>", spec, spec)
}

// JobPathHasEquals is the refusal for a continued job named by a directory
// path that contains '=': the character marks a voice's prompt file, so the
// path cannot be told apart from an attachment.
func JobPathHasEquals(spec string) string {
	return fmt.Sprintf("--with %s: a job directory path may not contain '=', which separates a voice from its prompt file (--with @<job>=<file>); move the job directory to a path without '=' and continue it from there", spec)
}

func RunNeedsVoice() string {
	return "at least one --with is required: a cold voice as provider[:model[:effort]] (codex, claude:opus, codex:gpt-6-astra:high), " +
		"or @<job> to continue a finished job's conversation"
}

// AllowWriteNeedsOneVoice explains why write intent belongs to a single
// voice: several turns editing one tree race each other.
func AllowWriteNeedsOneVoice() string {
	return "--allow-write needs exactly one --with: a fan-out's members share one working tree, and turns editing the " +
		"same files concurrently overwrite each other's work. For parallel write work, give each turn its own tree — " +
		"a git worktree per turn — and run them as separate jobs with --cwd <tree>."
}

// WriteSourceInRoster refuses to seat a write conversation beside other
// voices, whose members are read-only: silently narrowing the conversation's
// write intent is the exact drop the inheritance contract forbids.
func WriteSourceInRoster(dir string, timeoutMin float64) string {
	return fmt.Sprintf("--with @%s: that job ran with --allow-write, and a fan-out's members are read-only — "+
		"they share one working tree. Continue this conversation alone, keeping its write intent:\n  %s",
		dir, ContinueCommand(dir, timeoutMin))
}

// DuplicateConversation refuses a roster that names one conversation twice:
// a session admits a single live turn, so the second member could never
// dispatch.
func DuplicateConversation(session, firstDir, secondDir string) string {
	return fmt.Sprintf("the roster names the same conversation twice (session %s, from %s and %s), and a session "+
		"admits one live turn at a time. Name each conversation once.", session, firstDir, secondDir)
}

// CwdMix asks for an explicit tree when the continued jobs do not share one:
// a job's members run in a single working directory, and the engine will not
// choose between the recorded ones.
func CwdMix(firstDir, firstCwd, secondDir, secondCwd string) string {
	return fmt.Sprintf("the continued jobs ran in different working directories (%s ran in %s; %s ran in %s). "+
		"A job's members share one tree — pass --cwd to choose it.", firstDir, firstCwd, secondDir, secondCwd)
}

// BaselineMix asks for an explicit anchor when the continued jobs recorded
// different ones: a job reports one reviewed range, and the engine will not
// choose which conversation's anchor wins.
func BaselineMix(firstDir, firstBaseline, secondDir, secondBaseline string) string {
	return fmt.Sprintf("the continued jobs recorded different baselines (%s recorded %s; %s recorded %s). "+
		"A job reports one reviewed range — pass --baseline to choose the anchor.",
		firstDir, firstBaseline, secondDir, secondBaseline)
}

// ---------- fan-out ----------
//
// A fan-out is several turns, each on its own prompt, supervised as a single job. Its
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

// FanMemberNoRecord is a fan-out member's section at collect time when its
// directory holds no meta.json. Unlike the supervisor at dispatch time, collect
// did not watch the runner return, so it says only what the directory shows
// and sends the reader to the logs before any retry.
func FanMemberNoRecord() string {
	return "no record — this member never wrote meta.json, so whether anything ran for it is unknown"
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

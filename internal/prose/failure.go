package prose

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/text"
)

// Failure says why a turn did not deliver, worded from its record alone: the
// recorded cause and the facts the record holds beside it — the provider, the
// exit, the cap, the launcher. "" for a turn with no failure.
func Failure(m *job.Meta) string {
	f := m.Failure
	if f == nil {
		return ""
	}
	provider := capitalize(m.Provider)
	switch f.Cause {
	case job.CauseSpawnFailed:
		// The one failure where re-sending the same prompt is provably safe.
		return fmt.Sprintf("envoy could not start %s: %s", command(m), job.Deref(f.Message))
	case job.CauseProviderVerdict:
		return fmt.Sprintf("%s reported a provider failure: %s", provider, verdict(f))
	case job.CauseProviderRefusal:
		return refused(provider, f)
	case job.CauseBudgetCap:
		budget := 0.0
		if m.MaxBudgetUSD != nil {
			budget = *m.MaxBudgetUSD
		}
		return fmt.Sprintf("%s stopped at the --max-budget-usd %g cap after accepting the prompt.", provider, budget)
	case job.CauseExitedAfterResponse:
		return processExited(m, "after producing a response", "")
	case job.CauseExitedWithoutResult:
		detail := stderrDetail(f.StderrTail)
		if f.LastErrorEvent != nil {
			detail = fmt.Sprintf("last error event %q; stderr: %s", *f.LastErrorEvent, detail)
		}
		return processExited(m, "but returned no usable result", detail)
	case job.CauseExitedWithoutEnvelope:
		return processExited(m, "but returned no parseable result envelope", stderrDetail(f.StderrTail))
	case job.CauseForeignSignal:
		return fmt.Sprintf("%s was killed by signal %s, which envoy did not send.", command(m), job.Deref(m.ChildExitSignal))
	case job.CauseTimeout:
		var stream job.StreamSample
		if f.Stream != nil {
			stream = *f.Stream
		}
		return timedOut(m.TimeoutMin, m.Provider, stream)
	case job.CauseInterrupted:
		return fmt.Sprintf("envoy stopped %s after receiving %s.", m.Provider, job.Deref(m.InterruptionSignal))
	case job.CauseSessionConflict:
		return fmt.Sprintf("%s reported a session id that another turn already holds, so this turn was stopped.", provider)
	case job.CauseAbandoned:
		return "This turn ended without publishing a result: " + RunDetail(RunObservation{State: RunAbandoned}) + "."
	default:
		return fmt.Sprintf("This turn did not deliver (recorded cause %q).", f.Cause)
	}
}

// verdict is the provider's own words for a failure, or — when it gave none
// — that the turn failed, with the provider's code for it where there is one.
func verdict(f *job.Failure) string {
	switch {
	case f.Message != nil:
		return *f.Message
	case f.Code != nil:
		return fmt.Sprintf("turn failed (%s)", *f.Code)
	}
	return "turn failed"
}

// refused words a safety-classifier refusal from the two things the provider
// gave for it. The category leads because it is the part a caller can act on:
// it says which rule the classifier applied, where the provider's terminal
// message says only that something was flagged — and a caller reading that
// message goes looking for the offence in the prompt's subject matter.
//
// It carries no remedy. The record does not hold which words the classifier
// matched, or whether anything in the prompt was the reason, so a fix would
// be a guess — and one aimed at the prompt contradicts the recovery beside
// it, which continues the session on a new prompt.
func refused(provider string, f *job.Failure) string {
	line := provider + " refused this turn"
	if f.Code != nil {
		line += fmt.Sprintf(" (category %s)", *f.Code)
	}
	if f.Message != nil {
		return line + ": " + *f.Message
	}
	return line + "."
}

// remedy is the fix a cause demands before any recovery, "" when it needs
// none. It rides along with the prescription rather than replacing it.
func remedy(f *job.Failure) string {
	if f == nil {
		return ""
	}
	switch f.Cause {
	case job.CauseProviderVerdict:
		return "Fix the cause it reported first."
	case job.CauseBudgetCap:
		return "Raise the budget cap before continuing."
	}
	return ""
}

// FailedResult is result.md for a turn that did not deliver: its status, why,
// and any output recovered before the failure ("" for none).
func FailedResult(m *job.Meta, partial string) string {
	body := fmt.Sprintf("# Turn %s\n\n%s\n", m.Status, Failure(m))
	if partial != "" {
		body += fmt.Sprintf("\n## Partial output recovered before the failure\n\n%s\n", partial)
	}
	return body
}

// command is the executable the record says ran: the launcher when one
// replaced the bare provider, else the provider itself.
func command(m *job.Meta) string {
	if len(m.CommandPrefix) > 0 {
		return m.CommandPrefix[0]
	}
	return m.Provider
}

// processExited distinguishes an observed command exit from a provider
// verdict. A launcher can exit before the provider runs or remain as its
// parent, so a launched turn names the command, not the provider.
func processExited(m *job.Meta, observation, detail string) string {
	name, source := capitalize(m.Provider), "provider"
	if m.UsesLauncher() {
		name, source = fmt.Sprintf("Command %q", m.CommandPrefix[0]), "command"
	}
	codeText := "null"
	if m.ChildExitCode != nil {
		codeText = fmt.Sprintf("%d", *m.ChildExitCode)
	}
	line := fmt.Sprintf("%s exited with code %s %s.", name, codeText, observation)
	if detail != "" {
		line += fmt.Sprintf(" Last %s detail: %s", source, detail)
	}
	return line
}

// stderrDetail joins the stderr lines an exit left behind.
func stderrDetail(lines []string) string {
	if len(lines) == 0 {
		return "(no stderr detail)"
	}
	return strings.Join(lines, " | ")
}

// timedOut is the terminal envelope of a turn the wall-clock cap ended: what
// happened, what reaching the cap does and does not prove, and what the stream
// itself looked like on the way there.
//
// The stream is an observation, never a verdict. The cap stays a deadline
// rather than a stall detector — envoy still never stops a turn for being
// quiet — but the caller deciding what to do next needs the difference between
// a turn that was mid-sentence when time ran out and one that went silent
// seconds after the prompt was accepted; a cap alone reads the same either way.
// The clause reports and stops. It never concludes that no work happened:
// bytes can arrive that yield no event envoy can parse, a provider can work for
// minutes between events, and work can exist in the session and the tree that
// never reached this process at all. Saying otherwise would contradict the
// recovery line printed beside it, which is the part that actually licenses
// the caller's next move.
func timedOut(timeoutMin float64, provider string, stream job.StreamSample) string {
	line := fmt.Sprintf(
		"The %g-minute wall-clock cap ended this %s turn. The cap counts healthy work too, so reaching it is not evidence the provider hung.",
		timeoutMin, provider)
	switch {
	case stream.Events > 0 && stream.LastEvent != "":
		return line + fmt.Sprintf(" When the cap arrived the stream had been quiet for %s, after %s (last: %s).",
			text.FormatDuration(time.Duration(stream.QuietMs)*time.Millisecond), eventCount(stream.Events), stream.LastEvent)
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

// SessionLock words a session lock a turn could not take: who holds it and
// the one safe next step, or the error that stopped the attempt. A holder
// that is not provably live is still never reclaimed: its provider may
// outlive its runner, so recovery inspects first.
func SessionLock(c *job.LockConflict) string {
	if c.Error != nil {
		return *c.Error
	}
	if c.HolderLive && c.Holder != nil {
		return fmt.Sprintf("session %s already has a live turn (pid %d, started %s, job %s). One turn per session: wait for that job — %s — then collect it: %s",
			c.SessionID, c.Holder.Pid, c.Holder.StartedAt, c.Holder.Dir, WaitCommand(c.Holder.Dir), CollectCommand(c.Holder.Dir))
	}
	inspect := ""
	if c.Holder != nil && c.Holder.Dir != "" {
		inspect = fmt.Sprintf("Run %s and inspect its provider state. ", CollectCommand(c.Holder.Dir))
	}
	return fmt.Sprintf("session %s has an existing lock whose runner is not provably live (%s). "+
		"Automatic takeover is refused because its provider may still be running. "+
		"%sOnly after the provider is gone and its work is accounted for, remove the stale lock and retry.",
		c.SessionID, c.LockPath, inspect)
}

// LockedSession explains a session-id collision, which stops a turn rather
// than letting two live turns interleave one conversation.
func LockedSession(c *job.LockConflict, providerAlive bool) string {
	warning := ""
	if providerAlive {
		warning = " This job also has a provider process that may still be alive; stop or wait for it first."
	}
	return fmt.Sprintf("Another turn holds this session id: %s.%s Collect the job named there before touching this session — "+
		"two live turns on one conversation corrupt it.", strings.TrimRight(SessionLock(c), ". \t\n"), warning)
}

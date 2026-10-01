package prose

import (
	"fmt"
	"strings"

	"github.com/qiushiyan/envoy/internal/text"
)

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
// Kind is "" for a member that can continue; Detail carries the read error
// of an unreadable record.
type FanMemberCandidate struct {
	Name   string
	Dir    string
	Kind   ResumeBlockerKind
	Detail string
}

// GroupRefMustStandAlone refuses @<fan-out> mixed with other voices: a
// fan-out reference continues every member as one round, so beside other
// voices it would silently turn "continue the round" into roster
// composition. The eligible members are offered as the voices to name.
func GroupRefMustStandAlone(dir string, members []FanMemberCandidate) string {
	var offered, blocked []string
	for _, m := range members {
		if m.Kind != "" {
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

// RunNeedsVoice refuses a run with no member at all.
func RunNeedsVoice() string {
	return "at least one --with is required: a cold voice as provider[:model[:effort]] (codex, claude:opus, codex:gpt-6-astra:high), " +
		"or @<job> to continue a finished job's conversation"
}

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

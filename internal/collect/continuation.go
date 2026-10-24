package collect

import (
	"errors"
	"fmt"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
)

// What a turn's records license next: whether its conversation may continue,
// and the recovery its record prescribes when it did not deliver.

// continuation is what the one continuation rule says about a turn, read
// once wherever a surface needs it: whether its conversation may continue,
// and the state of its standing-instructions archive, which a surface reports
// and a retry repairs whatever else blocks the continuation.
type continuation struct {
	// blocker is why the conversation may not continue; "" = it may.
	blocker prose.ResumeBlockerKind
	// instructions is the text a continuation carries on ("" = none), read and
	// verified against the record; problem is what keeps the archive from
	// supplying it, "" when nothing does.
	instructions string
	problem      job.InstructionsProblem
}

// continuationOf is the single definition of "this job's conversation may
// not be continued", consulted by every surface that advertises or
// dispatches a continuation: collect's resume line, the fan-out's set-level
// resume line, and the @<job> inspection at dispatch. One definition means no
// surface can advertise a follow-up that dispatch would refuse. A recorded
// session-lock conflict blocks because the session may belong to another job.
// A conversation whose record names standing instructions continues only with
// them: every launch passes them again, so one without them would leave the
// session bare after its next compaction — and an archive that is gone or
// edited cannot supply them.
func continuationOf(dir string, meta *job.Meta) continuation {
	c := continuation{}
	c.instructions, c.problem = job.ReadInstructions(dir, meta)
	switch {
	case meta.Status == job.StatusRunning:
		c.blocker = prose.BlockerRunning
	case meta.SessionLockConflict != nil:
		c.blocker = prose.BlockerLockConflict
	case meta.SessionID == nil:
		c.blocker = prose.BlockerNoSession
	case c.problem == job.InstructionsMissing:
		c.blocker = prose.BlockerInstructionsMissing
	case c.problem == job.InstructionsChanged:
		c.blocker = prose.BlockerInstructionsChanged
	}
	return c
}

// Continuation is what a continuable turn hands the voice that continues it:
// the record it inherits its settings from, whose session is always set, and
// the standing instructions the conversation carries on ("" = none).
type Continuation struct {
	Meta         *job.Meta
	Instructions string
}

// Blocked is a finished turn whose conversation may not be continued: why,
// and — when its archive no longer supplies the standing instructions its
// record names — how to make it supply them again, worded from the same read.
type Blocked struct {
	Kind   prose.ResumeBlockerKind
	Repair string // "" unless the archive needs one
}

func (b *Blocked) Error() string {
	return fmt.Sprintf("the conversation cannot be continued (%s)", b.Kind)
}

// Continuable judges a turn record, as read (job.ReadRecord), as the source of
// a continuation, returning what a continuation inherits. A real turn that
// cannot continue is a *Blocked; any other error means there is no readable
// turn there at all.
func Continuable(r job.Record) (*Continuation, error) {
	if errors.Is(r.Err, job.ErrNoRecord) {
		return nil, errors.New("no job found there (no meta.json)")
	}
	if r.Err != nil {
		return nil, r.Err
	}
	meta := r.Meta
	if !job.KnownStatus(meta.Status) || meta.Provider == "" {
		return nil, fmt.Errorf("meta.json is not a job this engine wrote (status %q)", meta.Status)
	}
	c := continuationOf(r.Dir, meta)
	if c.blocker != "" {
		blocked := &Blocked{Kind: c.blocker}
		// The repair is a copy from an earlier turn of the conversation whose
		// archive still holds the text, else the digest a restored file must
		// match.
		if c.problem != "" {
			blocked.Repair = prose.InstructionsRepair(r.Dir, meta, job.IntactInstructionsSource(r.Dir, meta))
		}
		return nil, blocked
	}
	return &Continuation{Meta: meta, Instructions: c.instructions}, nil
}

// resumeCommand is the follow-up that continues this job's conversation, or
// "" when continuing it is not licensed — the same eligibility an @<job>
// voice applies, so collect can never offer a continuation dispatch would
// refuse.
func resumeCommand(dir string, meta *job.Meta, c continuation) string {
	if c.blocker != "" {
		return ""
	}
	return prose.ContinueCommand(dir, meta.TimeoutMin)
}

// recoveryAction is the prescription for a terminal job that did not
// deliver, rendered from its records: the lock refusal when the session was
// held, otherwise what the prompt state licenses, the remedy its recorded
// failure calls for, and the commands that continue or repeat the dispatch.
func recoveryAction(dir string, meta *job.Meta, c continuation) string {
	if meta.SessionLockConflict != nil {
		return prose.LockedSession(meta.SessionLockConflict, false)
	}
	// The identical retry of a turn that started its conversation sends the
	// archive as the new conversation's instructions, so an archive that no
	// longer holds what was sent is never offered as what was sent. A retried
	// continuation takes its instructions from its source, never from here.
	note := ""
	if meta.ResumedFrom == nil && c.problem != "" {
		note = prose.InstructionsRetryNote(meta)
	}
	return prose.Recovery(meta, resumeCommand(dir, meta, c), prose.RedispatchCommand(dir, meta, c.problem), note)
}

// recoveryForStale prescribes the next move for a job whose runner is gone.
// A live orphan provider outranks the prompt state: acting alongside it would
// put two turns in one tree.
func recoveryForStale(dir string, meta *job.Meta, state prose.RunObservation, c continuation) string {
	if meta.SessionLockConflict != nil {
		return prose.LockedSession(meta.SessionLockConflict, state.State == prose.RunOrphaned)
	}
	switch state.State {
	case prose.RunOrphaned:
		return prose.Orphaned()
	case prose.RunAbandoned:
		return recoveryAction(dir, meta, c)
	default:
		return prose.Unprovable()
	}
}

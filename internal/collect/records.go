package collect

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// What a turn's records license: whether its conversation may continue,
// whether it still holds its name, what its processes show while it is
// recorded as running, and the recovery its record prescribes.

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
	if meta == nil {
		return continuation{blocker: prose.BlockerUnreadableMeta}
	}
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

// Inspect reads dir as a single turn and reports whether its conversation
// can be continued; see Continuable.
func Inspect(dir string) (*Continuation, prose.ResumeBlockerKind, error) {
	meta, err := job.ReadMeta(dir)
	return Continuable(dir, meta, err)
}

// Continuable judges the turn record read from dir — the record and its read
// error — returning what a continuation inherits. The error is non-nil only
// when there is no readable turn at all; a real turn that cannot continue
// reports a blocker instead.
func Continuable(dir string, meta *job.Meta, err error) (*Continuation, prose.ResumeBlockerKind, error) {
	if errors.Is(err, job.ErrNoRecord) {
		return nil, "", errors.New("no job found there (no meta.json)")
	}
	if err != nil {
		return nil, "", err
	}
	if !job.KnownStatus(meta.Status) || meta.Provider == "" {
		return nil, "", fmt.Errorf("meta.json is not a job this engine wrote (status %q)", meta.Status)
	}
	c := continuationOf(dir, meta)
	if c.blocker != "" {
		return nil, c.blocker, nil
	}
	return &Continuation{Meta: meta, Instructions: c.instructions}, "", nil
}

// NameHold is the single definition of "this job still holds its name": it
// is undelivered — some turn in it not yet terminal and collected, which is
// what collectedAt means — so a caller may still be waiting to collect it by
// that name. Whom the hold binds is the dispatcher's question (a job holds
// its name only against a caller that shares its meaning of the name); this
// says only whether there is anything left to deliver. Age is not consulted:
// a job nobody collected holds until someone does, and pending lists it. A
// directory with no record holds too: it is a dispatch between its
// reservation and its first record, or one that died there, and the engine
// cannot tell which.
//
// A fan-out holds through any member that does. A member the roster names
// that has no record — a session held at dispatch refuses that member alone,
// before it writes anything — has nothing a collect could ever deliver, but
// its absence alone does not say it was refused: members start independently,
// and one may still be preparing its turn. What says it never will start is
// the supervising process being gone, so a recordless member holds for as
// long as that process may be alive, and an older manifest that does not name
// it holds for good.
func NameHold(dir string) (prose.NameHold, bool) {
	if !job.IsGroupDir(dir) {
		return turnHold(dir)
	}
	stamp, group, err := job.ReadGroupRecord(dir)
	var other *job.SchemaError
	switch {
	case errors.As(err, &other):
		return otherFanHold(dir, stamp)
	case err != nil:
		return prose.NameHold{Kind: prose.HoldUnreadable}, true
	}
	mayStillStart := proc.RunnerLiveness(dir, group.RunnerPid, group.RunnerLock) != proc.Gone
	for _, m := range job.ReadMembers(dir, group) {
		if errors.Is(m.Err, job.ErrNoRecord) {
			if mayStillStart {
				return prose.NameHold{Kind: prose.HoldUnrecorded, Member: m.Name}, true
			}
			continue
		}
		if hold, held := stampHold(m.Dir, m.Stamp); held {
			hold.Member = m.Name
			return hold, true
		}
	}
	return prose.NameHold{}, false
}

// otherFanHold is the hold another schema version's fan-out places on its
// name, decided from its stamp alone: like another version's turn, it holds
// only while its own engine may still be running it — the runner its stamp
// records, not provably gone. This engine can deliver nothing under that
// name, so a hold past the runner would strand it for good, and a manifest
// that records no runner gives this engine nothing to wait on.
func otherFanHold(dir string, stamp *job.GroupStamp) (prose.NameHold, bool) {
	if (stamp.RunnerPid > 0 || stamp.RunnerLock) && proc.RunnerLiveness(dir, stamp.RunnerPid, stamp.RunnerLock) != proc.Gone {
		return prose.NameHold{Kind: prose.HoldOtherVersion}, true
	}
	return prose.NameHold{}, false
}

func turnHold(dir string) (prose.NameHold, bool) {
	stamp, err := job.ReadStamp(dir)
	if errors.Is(err, job.ErrNoRecord) {
		entries, err := os.ReadDir(dir)
		return prose.NameHold{Kind: prose.HoldUnrecorded, Empty: err == nil && len(entries) == 0}, true
	}
	return stampHold(dir, stamp)
}

// stampHold is the hold a turn record's stamp places on its name; a nil stamp
// is a record that does not parse, which holds as unreadable.
//
// A record of another schema version holds only while its own engine may
// still be running it. That turn is live work under the name until its runner
// is gone; after that this engine cannot deliver the record, so no caller can
// be waiting to collect it by the name here — and since no collect of this
// engine can reconcile it either, holding past the runner would strand the
// name for good.
func stampHold(dir string, stamp *job.Stamp) (prose.NameHold, bool) {
	switch {
	case stamp == nil:
		return prose.NameHold{Kind: prose.HoldUnreadable}, true
	case !slices.Contains(job.MetaSchemas, stamp.SchemaVersion):
		if stamp.Status == job.StatusRunning && proc.RunnerLiveness(dir, stamp.RunnerPid, stamp.RunnerLock) != proc.Gone {
			return prose.NameHold{Kind: prose.HoldOtherVersion}, true
		}
		return prose.NameHold{}, false
	case stamp.Status == job.StatusRunning:
		return prose.NameHold{Kind: prose.HoldRunning}, true
	case stamp.CollectedAt == nil:
		return prose.NameHold{Kind: prose.HoldUncollected}, true
	}
	return prose.NameHold{}, false
}

// classifyRunning reads a turn recorded as running by what its processes
// show; a turn that is not running observes nothing.
func classifyRunning(dir string, meta *job.Meta) prose.RunObservation {
	if meta.Status != job.StatusRunning {
		return prose.RunObservation{}
	}
	runnerAlive := proc.RunnerLiveness(dir, meta.RunnerPid, meta.RunnerLock)
	pgid := 0
	if meta.ProviderPgid != nil {
		pgid = *meta.ProviderPgid
	}
	pid := 0
	if meta.ProviderPid != nil {
		pid = *meta.ProviderPid
	}
	groupAlive := proc.GroupLiveness(pgid)
	providerAlive := proc.PidLiveness(pid)

	switch {
	case runnerAlive == proc.Live:
		return prose.RunObservation{State: prose.RunLive, RunnerPid: meta.RunnerPid}
	case groupAlive == proc.Live || providerAlive == proc.Live:
		id := pgid
		if id == 0 {
			id = pid
		}
		return prose.RunObservation{State: prose.RunOrphaned, ProviderGroup: id}
	case runnerAlive == proc.Gone && (groupAlive == proc.Gone || (meta.ProviderPgid == nil && providerAlive == proc.Gone)):
		return prose.RunObservation{State: prose.RunAbandoned}
	}
	return prose.RunObservation{State: prose.RunUnknown}
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

// InstructionsRepair is how the archive in dir can be made to supply its
// conversation's standing instructions again, "" when it already does or the
// record names none: a copy from an earlier turn of the conversation whose
// archive still holds the text, else the digest a restored file must match.
func InstructionsRepair(dir string) string {
	meta, err := job.ReadMeta(dir)
	if err != nil || continuationOf(dir, meta).problem == "" {
		return ""
	}
	return prose.InstructionsRepair(dir, meta, job.IntactInstructionsSource(dir, meta))
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

// reconcileAbandoned rewrites a provably abandoned running job as terminal
// status "abandoned" so its evidence survives.
func reconcileAbandoned(dir string, meta *job.Meta, state prose.RunObservation) *job.Meta {
	if state.State != prose.RunAbandoned {
		return meta
	}
	ws := job.Workspace{Dir: dir}
	meta.Status = job.StatusAbandoned
	meta.ReconciledAt = job.Ptr(job.ISO(time.Now()))
	meta.Usage.End()
	meta.Failure = &job.Failure{Cause: job.CauseAbandoned}
	if _, err := os.Stat(ws.ResultPath()); os.IsNotExist(err) {
		job.WriteFileAtomic(ws.ResultPath(), []byte(prose.FailedResult(meta, "")))
	}
	meta.WriteFile(ws.MetaPath())
	return meta
}

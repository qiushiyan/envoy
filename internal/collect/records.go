package collect

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// What a turn's records license: whether its conversation may continue,
// whether it still holds its name, what its processes show while it is
// recorded as running, and the recovery its record prescribes.

// continuationBlocker is the single definition of "this job's session may
// not be continued", consulted by every surface that advertises or dispatches
// a continuation: collect's resume line, the fan-out's set-level resume line,
// and the @<job> inspection at dispatch. One definition means no surface can
// advertise a follow-up that dispatch would refuse. A recorded session-lock
// conflict blocks because the session may belong to another job.
func continuationBlocker(meta *job.Meta) (prose.ResumeBlockerKind, bool) {
	switch {
	case meta == nil:
		return prose.BlockerUnreadableMeta, true
	case meta.Status == job.StatusRunning:
		return prose.BlockerRunning, true
	case meta.SessionLockConflict != nil:
		return prose.BlockerLockConflict, true
	case meta.SessionID == nil:
		return prose.BlockerNoSession, true
	}
	return "", false
}

// Inspect reads dir as a single turn and reports whether its conversation
// can be continued; see Continuable.
func Inspect(dir string) (*job.Meta, prose.ResumeBlockerKind, error) {
	return Continuable(job.ReadMeta(dir))
}

// Continuable judges a turn record as read — the record and its read error —
// returning the record a continuation inherits its settings from, whose
// session is then always set. The error is non-nil only when there is no
// readable turn at all; a real turn that cannot continue reports a blocker
// instead.
func Continuable(meta *job.Meta, err error) (*job.Meta, prose.ResumeBlockerKind, error) {
	if errors.Is(err, job.ErrNoRecord) {
		return nil, "", errors.New("no job found there (no meta.json)")
	}
	if err != nil {
		return nil, "", err
	}
	if !job.KnownStatus(meta.Status) || meta.Provider == "" {
		return nil, "", fmt.Errorf("meta.json is not a job this engine wrote (status %q)", meta.Status)
	}
	if kind, blocked := continuationBlocker(meta); blocked {
		return nil, kind, nil
	}
	return meta, "", nil
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
	fan, err := job.ReadFan(dir)
	var other *job.SchemaError
	switch {
	case errors.As(err, &other):
		return otherFanHold(fan.Stamp)
	case err != nil:
		return prose.NameHold{Kind: prose.HoldUnreadable}, true
	}
	mayStillStart := proc.PidLiveness(fan.Group.RunnerPid) != proc.Gone
	for _, m := range fan.Members {
		if errors.Is(m.Err, job.ErrNoRecord) {
			if mayStillStart {
				return prose.NameHold{Kind: prose.HoldUnrecorded, Member: m.Name}, true
			}
			continue
		}
		if hold, held := stampHold(m.Stamp); held {
			hold.Member = m.Name
			return hold, true
		}
	}
	return prose.NameHold{}, false
}

// otherFanHold is the hold another schema version's fan-out places on its
// name: like another version's turn, it holds only while its own engine may
// still be running it — the runner its stamp records, not provably gone. This
// engine can deliver nothing under that name, so a hold past the runner would
// strand it for good, and a manifest that records no runner gives this engine
// nothing to wait on.
func otherFanHold(stamp *job.GroupStamp) (prose.NameHold, bool) {
	if stamp.RunnerPid > 0 && proc.PidLiveness(stamp.RunnerPid) != proc.Gone {
		return prose.NameHold{Kind: prose.HoldRunning}, true
	}
	return prose.NameHold{}, false
}

func turnHold(dir string) (prose.NameHold, bool) {
	stamp, err := job.ReadStamp(dir)
	if errors.Is(err, job.ErrNoRecord) {
		entries, err := os.ReadDir(dir)
		return prose.NameHold{Kind: prose.HoldUnrecorded, Empty: err == nil && len(entries) == 0}, true
	}
	return stampHold(stamp)
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
func stampHold(stamp *job.Stamp) (prose.NameHold, bool) {
	switch {
	case stamp == nil:
		return prose.NameHold{Kind: prose.HoldUnreadable}, true
	case stamp.SchemaVersion != job.MetaSchemaVersion:
		if stamp.Status == job.StatusRunning && proc.PidLiveness(stamp.RunnerPid) != proc.Gone {
			return prose.NameHold{Kind: prose.HoldRunning}, true
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
func classifyRunning(meta *job.Meta) prose.RunObservation {
	if meta.Status != job.StatusRunning {
		return prose.RunObservation{}
	}
	runnerAlive := proc.PidLiveness(meta.RunnerPid)
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
func resumeCommand(dir string, meta *job.Meta) string {
	if _, blocked := continuationBlocker(meta); blocked {
		return ""
	}
	return prose.ContinueCommand(dir, meta.TimeoutMin)
}

// recoveryAction is the prescription for a terminal job that did not
// deliver, rendered from its records: what the prompt state licenses, the
// driver's own remedy where it recorded one, and the commands that continue
// or repeat the dispatch.
func recoveryAction(dir string, meta *job.Meta) string {
	if meta.SessionLockConflict != nil {
		return prose.LockedSession(meta.SessionLockConflict, false)
	}
	return prose.Recovery(meta, resumeCommand(dir, meta), prose.RedispatchCommand(dir, meta))
}

// recoveryForStale prescribes the next move for a job whose runner is gone.
// A live orphan provider outranks the prompt state: acting alongside it would
// put two turns in one tree.
func recoveryForStale(dir string, meta *job.Meta, state prose.RunObservation) string {
	if meta.SessionLockConflict != nil {
		return prose.LockedSession(meta.SessionLockConflict, state.State == prose.RunOrphaned)
	}
	switch state.State {
	case prose.RunOrphaned:
		return prose.Orphaned()
	case prose.RunAbandoned:
		return recoveryAction(dir, meta)
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

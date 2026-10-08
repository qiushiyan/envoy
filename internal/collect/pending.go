package collect

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// Pending discovery is the recovery index for a notification that may have
// been missed: it skips provably live jobs, prints no results, and marks
// nothing collected.

// jobDirs lists job directories under base, sorted by name. A dir counts as a
// job once it holds a record — meta.json for a turn, group.json for a fan-out —
// since a record is what a reader can act on; a directory reserved and
// released without one was never a job. It returns the read error rather than
// swallowing it: a store that could not be read is not an empty store.
func jobDirs(base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if dir := filepath.Join(base, e.Name()); job.HasRecord(dir) {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// pendingItem is one job needing attention after a possibly missed
// notification: its label, why it needs attention, and the one next action.
type pendingItem struct {
	label, dir, why, next string
	// stop is the command that stops a job still running, which its
	// dispatch no longer can once the caller has stopped waiting on it.
	stop string
}

func pendingJobs(dirs []string, caller string) []pendingItem {
	var pending []pendingItem
	for _, dir := range dirs {
		var item pendingItem
		var ok bool
		if job.IsGroupDir(dir) {
			item, ok = pendingGroup(dir, caller)
		} else {
			stamp, meta, err := job.ReadRecord(dir)
			item, ok = pendingTurn(dir, stamp, meta, err, caller)
		}
		if ok {
			pending = append(pending, item)
		}
	}
	return pending
}

// pendingTurn classifies one turn record, as read (job.ReadRecord): still
// needing attention, or not. Collection reconciles an abandoned record and
// renders its recovery from the reconciled state, so the index only points
// there.
//
// A turn still running in its own process is listed only to a caller that
// could have been waiting on it — its dispatcher, or either side without an
// identity, as with a name hold — because that caller may have lost its
// dispatch to a session that ended. Anyone else's healthy turn is not theirs
// to wait on or collect.
func pendingTurn(dir string, stamp *job.Stamp, meta *job.Meta, err error, caller string) (pendingItem, bool) {
	var other *job.SchemaError
	switch {
	case errors.Is(err, job.ErrNoRecord):
		return pendingItem{}, false
	case errors.As(err, &other):
		// Another engine version's record is not damage. Its stamp says
		// whether anything is still owed — skipped while its own engine is
		// provably running it, like any live job — and this engine reads no
		// further.
		if stamp.Status == job.StatusRunning && proc.RunnerLiveness(dir, stamp.RunnerPid, stamp.RunnerLock) == proc.Live {
			return pendingItem{}, false
		}
		if stamp.Status != job.StatusRunning && stamp.CollectedAt != nil {
			return pendingItem{}, false
		}
		return pendingItem{label: "other-schema", dir: dir,
			why: prose.PendingOtherSchema(other.Record, other.Version, other.Reads), next: prose.PendingOtherSchemaNext(dir)}, true
	case err != nil:
		return pendingItem{label: "corrupt", dir: dir,
			why: prose.PendingUnreadable("meta.json", err), next: prose.PendingUnreadableNext(dir)}, true
	case meta.Status == job.StatusRunning:
		state := classifyRunning(dir, meta)
		if state.State == prose.RunLive {
			if !mayAwait(caller, stamp.Caller) {
				return pendingItem{}, false
			}
			return pendingItem{label: "running", dir: dir, why: prose.PendingRunning(prose.RunDetail(state)), next: prose.PendingRunningNext(dir),
				stop: prose.StopCommand(state.RunnerPid)}, true
		}
		next := prose.CollectCommand(dir)
		if state.State != prose.RunAbandoned {
			next = recoveryForStale(dir, meta, state)
		}
		return pendingItem{label: string(state.State), dir: dir, why: prose.RunDetail(state), next: next}, true
	case meta.CollectedAt == nil:
		return pendingItem{label: "terminal:" + meta.Status, dir: dir,
			why: prose.PendingUncollected(meta.Status), next: prose.CollectCommand(dir)}, true
	}
	return pendingItem{}, false
}

// pendingGroup rolls a fan-out up as the single entry a caller acts on: one
// collect covers every member. Discovery names which members need attention and
// why; the prescription for each of them is per member, and printing it is
// collect's job, not the index's.
func pendingGroup(dir, caller string) (pendingItem, bool) {
	stamp, group, err := job.ReadGroupRecord(dir)
	var other *job.SchemaError
	switch {
	case errors.As(err, &other):
		return pendingOtherFan(dir, stamp, other, caller)
	case err != nil:
		return pendingItem{label: "corrupt", dir: dir,
			why: prose.PendingUnreadable("group.json", err), next: prose.PendingUnreadableNext(dir)}, true
	}
	// Every member runs in the fan-out's one process, so while it lives the
	// fan-out is one running job, listed like a running turn.
	if proc.RunnerLiveness(dir, group.RunnerPid, group.RunnerLock) == proc.Live {
		if !mayAwait(caller, group.Caller) {
			return pendingItem{}, false
		}
		return pendingItem{label: "running", dir: dir, why: prose.PendingRunning(prose.PendingFanAlive()), next: prose.PendingRunningNext(dir),
			stop: prose.StopCommand(group.RunnerPid)}, true
	}
	members := job.ReadMembers(dir, group)
	var reasons []string
	for _, m := range members {
		// A member the roster names but that never wrote a record is the one
		// case the member's own files cannot report; the roster is the only
		// evidence it was meant to run, so the group carries it.
		if errors.Is(m.Err, job.ErrNoRecord) {
			reasons = append(reasons, m.Name+": "+prose.PendingMemberNoRecord())
			continue
		}
		if item, ok := pendingTurn(m.Dir, m.Stamp, m.Meta, m.Err, caller); ok {
			reasons = append(reasons, m.Name+": "+item.why)
		}
	}
	if len(reasons) == 0 {
		return pendingItem{}, false
	}
	return pendingItem{label: "group", dir: dir,
		why: prose.PendingMembers(reasons, len(members)), next: prose.CollectCommand(dir)}, true
}

// pendingOtherFan classifies another engine version's fan-out, which is not
// damage either. Its stamp says whether its own engine is provably still
// running it — skipped then, like any live job — and past that, the stamps of
// the members its directory holds records for say whether anything is owed;
// a directory that cannot be listed cannot show that nothing is. This engine
// reads no further, so the entry names the files, not a member.
func pendingOtherFan(dir string, stamp *job.GroupStamp, other *job.SchemaError, caller string) (pendingItem, bool) {
	if proc.RunnerLiveness(dir, stamp.RunnerPid, stamp.RunnerLock) == proc.Live {
		return pendingItem{}, false
	}
	item := pendingItem{label: "other-schema", dir: dir,
		why: prose.PendingOtherSchema(other.Record, other.Version, other.Reads), next: prose.PendingOtherSchemaFanNext(dir)}
	members, err := job.RecordedMembers(dir)
	if err != nil {
		return item, true
	}
	for _, m := range members {
		if _, owed := pendingTurn(m.Dir, m.Stamp, m.Meta, m.Err, caller); owed {
			return item, true
		}
	}
	return pendingItem{}, false
}

// Pending prints the discovery-only recovery index: it skips provably live
// jobs, does not print full results, and does not mark anything collected.
//
// caller is the identity of the session asking, "" for none.
func Pending(base string, baseWasDerived bool, caller string, w, errW io.Writer) int {
	// "no recovery action is needed" over a store that could not be read is the
	// most reassuring thing this command can say and the least earned. The one
	// benign error is a derived store that does not exist yet — a project
	// before its first dispatch creates it. The same error on a --base the
	// caller typed means it named something that isn't there.
	dirs, err := jobDirs(base)
	firstRun := baseWasDerived && errors.Is(err, fs.ErrNotExist)
	if err != nil && !firstRun {
		fmt.Fprintf(errW, "pending error: %s\n", prose.UnreadableStore(base, err))
		return job.ExitInfra
	}
	pending := pendingJobs(dirs, caller)
	fmt.Fprintf(w, "pending jobs: %d (under %s)\n", len(pending), base)
	if len(pending) == 0 {
		fmt.Fprintf(w, "next: %s\n", prose.PendingNone())
		return 0
	}
	for _, item := range pending {
		fmt.Fprintf(w, "\n[%s] %s\n", item.label, item.dir)
		fmt.Fprintf(w, "why: %s\n", item.why)
		fmt.Fprintf(w, "next: %s\n", item.next)
		if item.stop != "" {
			fmt.Fprintf(w, "stop: %s\n", item.stop)
		}
	}
	return 0
}

// mayAwait is whether a caller could have been waiting on a job its owner
// dispatched: the same caller, or either side without an identity — the
// sharing rule a name hold binds by.
func mayAwait(caller string, owner *string) bool {
	o := job.Deref(owner)
	return caller == "" || o == "" || o == caller
}

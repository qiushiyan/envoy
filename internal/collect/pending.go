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
}

func pendingJobs(dirs []string) []pendingItem {
	var pending []pendingItem
	for _, dir := range dirs {
		var item pendingItem
		var ok bool
		if job.IsGroupDir(dir) {
			item, ok = pendingGroup(dir)
		} else {
			meta, err := job.ReadMeta(dir)
			item, ok = pendingTurn(dir, meta, err)
		}
		if ok {
			pending = append(pending, item)
		}
	}
	return pending
}

// pendingTurn classifies one turn record, as read: still needing attention,
// or not. Collection reconciles an abandoned record and renders its recovery
// from the reconciled state, so the index only points there.
func pendingTurn(dir string, meta *job.Meta, err error) (pendingItem, bool) {
	switch {
	case errors.Is(err, job.ErrNoRecord):
		return pendingItem{}, false
	case err != nil:
		return pendingItem{label: "corrupt", dir: dir,
			why: prose.PendingUnreadable("meta.json", err), next: prose.PendingUnreadableNext(dir)}, true
	case meta.Status == job.StatusRunning:
		state := classifyRunning(meta)
		if state.State == prose.RunLive {
			return pendingItem{}, false
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
func pendingGroup(dir string) (pendingItem, bool) {
	fan, err := job.ReadFan(dir)
	if err != nil {
		return pendingItem{label: "corrupt", dir: dir,
			why: prose.PendingUnreadable("group.json", err), next: prose.PendingUnreadableNext(dir)}, true
	}
	var reasons []string
	for _, m := range fan.Members {
		// A member the roster names but that never wrote a record is the one
		// case the member's own files cannot report; the roster is the only
		// evidence it was meant to run, so the group carries it.
		if errors.Is(m.Err, job.ErrNoRecord) {
			reasons = append(reasons, m.Name+": "+prose.PendingMemberNoRecord())
			continue
		}
		if item, ok := pendingTurn(m.Dir, m.Meta, m.Err); ok {
			reasons = append(reasons, m.Name+": "+item.why)
		}
	}
	if len(reasons) == 0 {
		return pendingItem{}, false
	}
	return pendingItem{label: "group", dir: dir,
		why: prose.PendingMembers(reasons, len(fan.Members)), next: prose.CollectCommand(dir)}, true
}

// Pending prints the discovery-only recovery index: it skips provably live
// jobs, does not print full results, and does not mark anything collected.
func Pending(base string, baseWasDerived bool, w, errW io.Writer) int {
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
	pending := pendingJobs(dirs)
	fmt.Fprintf(w, "pending jobs: %d (under %s)\n", len(pending), base)
	if len(pending) == 0 {
		fmt.Fprintf(w, "next: %s\n", prose.PendingNone())
		return 0
	}
	for _, item := range pending {
		fmt.Fprintf(w, "\n[%s] %s\n", item.label, item.dir)
		fmt.Fprintf(w, "why: %s\n", item.why)
		fmt.Fprintf(w, "next: %s\n", item.next)
	}
	return 0
}

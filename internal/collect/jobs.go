package collect

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
	"github.com/qiushiyan/envoy/internal/text"
)

// jobsListLimit caps how many jobs one listing names. A project accumulates
// turns for as long as it is worked on, and the caller reading this is almost
// always looking for a recent one; the header says how many were left out so
// the cap never hides that older jobs exist.
const jobsListLimit = 20

// Jobs prints this project's job roster, newest first — the recovery path for
// an out-dir a caller no longer has.
//
// It is a read, like pending: nothing is stamped, nothing is reconciled, and
// no job's result is printed. Each row names the one coordinate collect takes,
// which is the whole point of the command.
func Jobs(base string, w io.Writer) int {
	dirs := JobDirs(base)
	total := len(dirs)
	if total == 0 {
		fmt.Fprintf(w, "%s\n", prose.JobsHeader(0, 0, base))
		fmt.Fprintf(w, "next: %s\n", prose.JobsNone())
		return 0
	}
	// JobDirs sorts by name, and the name begins with the dispatch stamp, so
	// reversing it puts the job a caller most likely wants first.
	newest := make([]string, 0, total)
	for i := len(dirs) - 1; i >= 0; i-- {
		newest = append(newest, dirs[i])
	}
	if len(newest) > jobsListLimit {
		newest = newest[:jobsListLimit]
	}
	fmt.Fprintln(w, prose.JobsHeader(len(newest), total, base))
	now := time.Now()
	for _, dir := range newest {
		fmt.Fprintln(w, jobRow(dir, now))
	}
	fmt.Fprintf(w, "next: %s\n", prose.JobsNext())
	return 0
}

// jobRow renders one job as a single line: its status, the coordinate collect
// takes, and the few facts that tell two same-day jobs apart.
func jobRow(dir string, now time.Time) string {
	if job.IsGroupDir(dir) {
		return groupRow(dir, now)
	}
	meta, _, err := job.ReadMetaFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return fmt.Sprintf("[unreadable] %s · meta.json could not be read (%s)", dir, err)
	}
	running := meta.Status == job.StatusRunning
	facts := []string{meta.Provider}
	if meta.Model != nil {
		facts = append(facts, *meta.Model)
	}
	facts = append(facts, elapsed(running, meta.DurationMs, meta.StartedAt, now))
	if !running {
		facts = append(facts, delivery(meta.CollectedAt != nil))
	}
	return row(meta.Status, dir, facts)
}

// groupRow renders a fan-out. Its status is the aggregate of its members' own
// statuses, read from the members themselves — group.json records a roster,
// never member state, so this asks the same source collect does.
func groupRow(dir string, now time.Time) string {
	group, err := job.ReadGroupFile(job.GroupWorkspace{Dir: dir}.GroupPath())
	if err != nil {
		return fmt.Sprintf("[unreadable] %s · group.json could not be read (%s)", dir, err)
	}
	statuses := make([]string, 0, len(group.Members))
	collected := len(group.Members) > 0
	for _, m := range group.Members {
		meta, _, err := job.ReadMetaFile(filepath.Join(m.OutDir, "meta.json"))
		if err != nil {
			statuses = append(statuses, job.StatusAbandoned)
			collected = false
			continue
		}
		statuses = append(statuses, meta.Status)
		if meta.CollectedAt == nil {
			collected = false
		}
	}
	status := prose.FanStatus(statuses)
	running := status == prose.FanRunning
	facts := []string{fmt.Sprintf("fan-out of %d", len(group.Members))}
	var durationMs *int64
	if group.EndedAt != nil {
		if started, err1 := time.Parse(time.RFC3339, group.StartedAt); err1 == nil {
			if ended, err2 := time.Parse(time.RFC3339, *group.EndedAt); err2 == nil {
				durationMs = job.Ptr(ended.Sub(started).Milliseconds())
			}
		}
	}
	facts = append(facts, elapsed(running, durationMs, group.StartedAt, now))
	if !running {
		facts = append(facts, delivery(collected))
	}
	return row(status, dir, facts)
}

func row(status, dir string, facts []string) string {
	line := fmt.Sprintf("[%s] %s", status, dir)
	for _, f := range facts {
		if f != "" {
			line += " · " + f
		}
	}
	return line
}

// elapsed renders how long a job took, or how long a still-running one has
// been going — the difference a caller scanning for "the one I just started"
// reads first.
func elapsed(running bool, durationMs *int64, startedAt string, now time.Time) string {
	if running {
		if t, err := time.Parse(time.RFC3339, startedAt); err == nil {
			return "started " + text.FormatDuration(now.Sub(t)) + " ago"
		}
		return "started " + startedAt
	}
	if durationMs == nil {
		return ""
	}
	return text.FormatDuration(time.Duration(*durationMs) * time.Millisecond)
}

// delivery says whether the result reached a caller, in collect's own
// vocabulary: a job stays owed until its result was actually printed.
func delivery(collected bool) string {
	if collected {
		return "collected"
	}
	return "owed"
}

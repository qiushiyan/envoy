package collect

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// waitPoll is how often a wait re-reads a runner known only by its PID; a
// runner that recorded its claim on the directory is waited on through the
// claim itself, with no polling. Env-overridable for the integration suite.
var waitPoll = func() time.Duration {
	if ms, err := strconv.ParseFloat(os.Getenv("ENVOY_WAIT_POLL_MS"), 64); err == nil && ms > 0 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	return time.Second
}()

// Wait blocks while the process supervising a job is alive, then prints the
// block the job's own dispatch prints when it ends and returns the dispatch's
// exit code. It is how a caller that lost its dispatch's process — a session
// that ended, a background task that was stopped — is told again when the
// turn ends.
//
// A wait delivers nothing: it prints no result and stamps nothing, because a
// background command's output reaches no reader. The block it prints sends the
// caller to collect, which delivers. It reconciles nothing either; a turn
// whose process ended without a final record is reported as such, and collect
// decides what that record means.
func Wait(dir, note string, w, errW io.Writer) int {
	var block string
	var code int
	if job.IsGroupDir(dir) {
		block, code = waitGroup(dir, errW)
	} else {
		block, code = waitTurn(dir, errW)
	}
	if block == "" {
		return code
	}
	fmt.Fprintf(w, "job: %s\n", dir)
	if note != "" {
		fmt.Fprintf(w, "note: %s\n", note)
	}
	fmt.Fprint(w, block)
	return code
}

func waitTurn(dir string, errW io.Writer) (string, int) {
	var stamp *job.Stamp
	var meta *job.Meta
	var err error
	for {
		stamp, meta, err = job.ReadRecord(dir)
		if stamp == nil || !turnLive(dir, stamp) {
			break
		}
		pause(dir, stamp.RunnerLock)
	}
	if err != nil {
		reportUnreadable(dir, err, errW)
		return "", job.ExitUsage
	}
	if meta.Status == job.StatusRunning {
		return fmt.Sprintf("\nstatus: %s\nnext: %s\n", prose.RunningStatus(classifyRunning(dir, meta)), prose.EndedUnrecorded(dir)),
			job.ExitInfra
	}
	return prose.TurnEnded(dir, meta.Status, job.Deref(meta.SessionID)), job.ExitCodeFor(meta.Status)
}

func waitGroup(dir string, errW io.Writer) (string, int) {
	for {
		stamp, _, err := job.ReadGroupRecord(dir)
		if stamp == nil || !groupLive(dir, stamp, err) {
			break
		}
		pause(dir, stamp.RunnerLock)
	}
	fan, err := job.ReadFan(dir)
	if err != nil {
		fmt.Fprintf(errW, "wait error: %s could not be read (%s). Each member's job dir under %s is self-contained — collect one directly to see its result.\n",
			job.GroupWorkspace{Dir: dir}.GroupPath(), err, dir)
		return "", job.ExitUsage
	}
	members := make([]prose.FanMemberEnd, len(fan.Members))
	codes := make([]int, len(fan.Members))
	for i, m := range fan.Members {
		members[i] = prose.FanMemberEnd{Name: m.Name, Dir: m.Dir}
		// A member with no record never started a provider: the runner
		// writes its first record before it spawns one. Which refusal
		// stopped it is not recorded, so its code is the dispatch's default.
		codes[i] = job.ExitInfra
		if m.Err == nil {
			members[i].Status = m.Meta.Status
			codes[i] = job.ExitCodeFor(m.Meta.Status)
		}
	}
	return prose.FanEnded(dir, members), job.ExitCodeForGroup(codes)
}

// turnLive is whether a turn's runner is still running it. A claimed runner
// is alive exactly while its claim is held — past its final record, until it
// has released the session lock too. One known only by its PID is alive while
// its record says running and the PID answers; a finished record ends the
// wait whatever the PID, which a later process may have reused.
func turnLive(dir string, stamp *job.Stamp) bool {
	if stamp.RunnerLock {
		return proc.DirLockLiveness(dir) == proc.Live
	}
	return stamp.Status == job.StatusRunning && proc.PidLiveness(stamp.RunnerPid) == proc.Live
}

// groupLive is whether the process supervising a fan-out is still running
// it, read the way turnLive reads a turn: its claim when it recorded one,
// otherwise its PID while some member is still recorded as running.
func groupLive(dir string, stamp *job.GroupStamp, readErr error) bool {
	if stamp.RunnerLock {
		return proc.DirLockLiveness(dir) == proc.Live
	}
	var other *job.SchemaError
	if stamp.RunnerPid <= 0 || proc.PidLiveness(stamp.RunnerPid) != proc.Live {
		return false
	}
	if errors.As(readErr, &other) {
		// Another version's roster is not read; its members are found by
		// the records they hold.
		members, err := job.RecordedMembers(dir)
		if err != nil {
			return false
		}
		for _, m := range members {
			if m.Stamp != nil && m.Stamp.Status == job.StatusRunning {
				return true
			}
		}
		return false
	}
	fan, err := job.ReadFan(dir)
	if err != nil {
		return false
	}
	for _, m := range fan.Members {
		if m.Err != nil || m.Meta.Status == job.StatusRunning {
			return true
		}
	}
	return false
}

// pause waits for the next look at a live runner: until its claim is
// released when it holds one, otherwise one poll interval.
func pause(dir string, claimed bool) {
	if claimed && proc.AwaitDirUnlock(dir) == nil {
		return
	}
	time.Sleep(waitPoll)
}

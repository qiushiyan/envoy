package collect

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// waitPoll is how often a wait re-reads a runner known only by its PID; a
// runner that recorded its claim on the directory is waited on through the
// claim itself, with no polling.
var waitPoll = time.Second

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
func Wait(ref string, w, errW io.Writer) int {
	dir, note, code, ok := resolve(ref, "wait", "wait takes the job to wait for: the name it was run as, or its directory", errW)
	if !ok {
		return code
	}
	return waitDir(dir, note, w, errW)
}

// waitDir waits for the job in dir, under the note its name resolved with.
func waitDir(dir, note string, w, errW io.Writer) int {
	// The job is named before the wait blocks, so whoever runs it sees at
	// once which job it waits on — and, from the note, when the name meant
	// another session's. A coordinate delivers nothing.
	fmt.Fprintf(w, "job: %s\n", dir)
	if note != "" {
		fmt.Fprintf(w, "note: %s\n", note)
	}
	var block string
	var code int
	if job.IsGroupDir(dir) {
		block, code = waitGroup(dir, errW)
	} else {
		block, code = waitTurn(dir, errW)
	}
	fmt.Fprint(w, block)
	return code
}

func waitTurn(dir string, errW io.Writer) (string, int) {
	awaitRunner(dir, func() (live, claimed bool) {
		stamp, err := job.ReadStamp(dir)
		if err != nil {
			return false, false
		}
		return turnLive(dir, stamp), stamp.RunnerLock
	})
	end := job.ReadRecord(dir)
	switch {
	case end.Err != nil:
		reportUnreadable(dir, end.Err, errW)
		return "", job.ExitUsage
	case !ended(end):
		return fmt.Sprintf("\nstatus: %s\nnext: %s\n", prose.RunningStatus(classifyRunning(dir, end.Meta)), prose.EndedUnrecorded(dir)),
			job.ExitInfra
	}
	return prose.TurnEnded(dir, end.Meta.Status, job.Deref(end.Meta.SessionID)), job.ExitCodeFor(end.Meta.Status)
}

// waitGroup waits for a fan-out's process, then reads each member's ending
// the way waitTurn reads a turn's. The dispatch's ending block describes a
// set of members that each ended or never started; a member still recorded
// as running, or one whose record will not read, did neither, so such a set
// is reported as an ending the records do not hold, for collect to read.
func waitGroup(dir string, errW io.Writer) (string, int) {
	awaitRunner(dir, func() (live, claimed bool) {
		stamp, _, err := job.ReadGroupRecord(dir)
		if stamp == nil {
			return false, false
		}
		return groupLive(dir, stamp, err), stamp.RunnerLock
	})
	fan, err := job.ReadFan(dir)
	if err != nil {
		reportUnreadableManifest("wait", dir, err, errW)
		return "", job.ExitUsage
	}
	members := make([]prose.FanMemberEnd, len(fan.Members))
	codes := make([]int, len(fan.Members))
	var unended []string
	for i, m := range fan.Members {
		members[i] = prose.FanMemberEnd{Name: m.Name, Dir: m.Dir}
		switch {
		case ended(m.Record):
			members[i].Status = m.Meta.Status
			codes[i] = job.ExitCodeFor(m.Meta.Status)
		case neverStarted(m.Record):
			// Which refusal stopped it is not recorded, so its code is the
			// dispatch's default for a member that published no status.
			codes[i] = job.ExitInfra
		default:
			unended = append(unended, m.Name)
		}
	}
	if len(unended) > 0 {
		return fmt.Sprintf("\nstatus: %s\nnext: %s\n", prose.FanEndedUnrecorded(unended), prose.FanEndedUnrecordedNext(dir)), job.ExitInfra
	}
	return prose.FanEnded(dir, members), job.ExitCodeForGroup(codes)
}

// ended is whether a turn's record, read once its runner is gone, says how
// the turn ended.
func ended(r job.Record) bool { return r.Err == nil && r.Meta.Status != job.StatusRunning }

// neverStarted is whether the turn never wrote a record. A runner writes its
// first record before it spawns a provider, so once its process is gone,
// nothing ran for it.
func neverStarted(r job.Record) bool { return errors.Is(r.Err, job.ErrNoRecord) }

// turnLive is whether a turn's runner is still running it. A claimed runner
// is alive exactly while its claim is held — past its final record, until it
// has released the session lock too. One known only by its PID is trusted
// only while its record says running: a finished runner's PID may since have
// been reused.
func turnLive(dir string, stamp *job.Stamp) bool {
	if proc.RunnerLiveness(dir, stamp.RunnerPid, stamp.RunnerLock) != proc.Live {
		return false
	}
	return stamp.RunnerLock || stamp.Status == job.StatusRunning
}

// groupLive is whether the process supervising a fan-out is still running
// it, read the way turnLive reads a turn: by its claim when it recorded one,
// otherwise by its PID while some member is still recorded as running.
func groupLive(dir string, stamp *job.GroupStamp, readErr error) bool {
	if proc.RunnerLiveness(dir, stamp.RunnerPid, stamp.RunnerLock) != proc.Live {
		return false
	}
	if stamp.RunnerLock {
		return true
	}
	if _, ok := errors.AsType[*job.SchemaError](readErr); ok {
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

// awaitRunner blocks while look reports the runner alive: on its claim when
// it holds one, otherwise one poll interval at a time.
func awaitRunner(dir string, look func() (live, claimed bool)) {
	for {
		live, claimed := look()
		if !live {
			return
		}
		if claimed && proc.AwaitDirUnlock(dir) == nil {
			continue
		}
		time.Sleep(waitPoll)
	}
}

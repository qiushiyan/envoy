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
func Wait(ref string, w, errW io.Writer) (code int, err error) {
	dir, note, code, err := resolve(ref)
	if err != nil {
		return code, err
	}
	return waitDir(dir, note, w, errW), nil
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
	awaitRunner(dir, func() (proc.Runner, bool) {
		stamp, err := job.ReadStamp(dir)
		if err != nil {
			return proc.Runner{}, false
		}
		return stamp.Runner(dir), true
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
	awaitRunner(dir, fanRunner(dir))
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

// fanRunner reads what a fan-out's records say about the process supervising
// it, as of each look: its manifest's stamp, and — when the manifest is this
// engine's — every member its roster names, which alone can establish that
// the work is finished. A manifest that does not parse names no process.
func fanRunner(dir string) func() (proc.Runner, bool) {
	return func() (proc.Runner, bool) {
		stamp, group, _ := job.ReadGroupRecord(dir)
		switch {
		case stamp == nil:
			return proc.Runner{}, false
		case group == nil:
			return stamp.Runner(dir), true
		}
		fan := job.Fan{Group: group, Members: job.ReadMembers(dir, group)}
		return fan.Runner(dir), true
	}
}

// awaitRunner blocks while the runner look reads from the records is alive
// (proc.RunnerLiveness): on its claim when it recorded one, otherwise one poll
// interval at a time. A look whose records name no runner ends the wait.
func awaitRunner(dir string, look func() (proc.Runner, bool)) {
	for {
		runner, named := look()
		if !named || proc.RunnerLiveness(runner) != proc.Live {
			return
		}
		if runner.Claimed && proc.AwaitDirUnlock(dir) == nil {
			continue
		}
		time.Sleep(waitPoll)
	}
}

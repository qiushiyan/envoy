package prose

import (
	"fmt"

	"github.com/qiushiyan/envoy/internal/text"
)

// A dispatch runs its turn in a process of its own, and the `envoy run` the
// caller started only waits for it. These are the waiter's own sentences:
// what happened to the waiting, never to the turn, which the records report.

// StoppedWaiting is what a waiter says when the caller's environment stops
// it: the waiting ended and the turn did not, with the commands that wait for
// it again and that stop it. jobRef is the job as the caller named it.
func StoppedWaiting(signalName, jobRef string) string {
	msg := fmt.Sprintf("envoy: received %s, so this command stopped waiting; the turn keeps running in its own process, to its end or its cap.", signalName)
	if jobRef == "" {
		return msg
	}
	ref := text.ShellQuote(jobRef)
	return msg + " Wait for it again: envoy wait " + ref + ". To stop the turn itself, run the stop: command that envoy collect --status-only " + ref + " prints."
}

// DispatchLost is what a waiter says when the process running the dispatch
// ended without reporting its exit code. Whether it reserved a job is what
// its "job:" line shows; collecting the name without one would read an
// earlier job.
func DispatchLost() string {
	return "envoy: the process running this dispatch ended without reporting how the dispatch went. " +
		"If a job: line appears above, collect that job for what it recorded; if none does, nothing was dispatched."
}

// DetachFailed is what a dispatch says when the process that runs it cannot
// be started.
func DetachFailed(err error) string {
	return fmt.Sprintf("envoy: cannot start the process that runs this dispatch (%s), so nothing was dispatched.", err)
}

// StopReachesEveryMember says what a member's stop reaches: every member of
// its fan-out runs in one process, so none can be stopped alone.
func StopReachesEveryMember(groupDir string) string {
	return "this member runs in its fan-out's one process, so that stop interrupts every member of the fan-out " + text.ShellQuote(groupDir) + "; one member cannot be stopped alone"
}

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
// ended without reporting its exit code.
func DispatchLost(jobRef string) string {
	msg := "envoy: the process running this dispatch ended without reporting how the dispatch went."
	if jobRef == "" {
		return msg
	}
	return msg + " Collect the job for what it recorded: " + CollectCommand(jobRef) + "."
}

// DetachUnavailable is what a dispatch says when it cannot start a process of
// its own and runs in the caller's instead.
func DetachUnavailable(err error) string {
	return fmt.Sprintf("envoy: cannot run this dispatch in a process of its own (%s), so it runs in this one and stops if this command is stopped.", err)
}

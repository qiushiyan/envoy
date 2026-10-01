package runner

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
	"github.com/qiushiyan/envoy/internal/provider"
)

// afterFunc schedules fn onto the event loop, so timer callbacks share the
// single-threaded state machine.
func (r *run) afterFunc(d time.Duration, fn func()) *time.Timer {
	return time.AfterFunc(d, func() {
		select {
		case r.callCh <- fn:
		default: // loop already finished; the callback is moot
		}
	})
}

// stopReason is why the runner, rather than the provider, ended a turn.
type stopReason int

const (
	stopTimeout stopReason = iota + 1
	stopInterrupted
	stopLockConflict
)

// escalation is how far stopping the provider tree has gone. Each step
// replaces the one timer that schedules the next.
type escalation int

const (
	notStopping escalation = iota
	terminating            // the tree was asked to stop; SIGKILL follows after sigkillAfter
	killing                // SIGKILL sent; finalize after closeGrace
)

// requestTermination starts a stop the runner decided on: TERM the tree (INT
// for an interrupt that was an INT), escalate to KILL, then give streams one
// final drain window before publishing terminal metadata. received is the
// signal that asked for an interrupt, nil otherwise.
func (r *run) requestTermination(reason stopReason, received os.Signal) {
	if r.done {
		return
	}
	if r.term != nil {
		// A second interrupt is an explicit request to stop waiting for cleanup.
		if reason == stopInterrupted {
			r.kill()
		}
		return
	}
	requestedAt := time.Now()
	r.term = &termination{
		reason:      reason,
		requestedAt: requestedAt,
		stream:      r.capStream(requestedAt),
	}
	progressReason, treeSignal := "SIGTERM", syscall.SIGTERM
	switch reason {
	case stopTimeout:
		progressReason = "hard_cap"
	case stopInterrupted:
		r.term.signal = proc.SignalName(received)
		progressReason = r.term.signal
		if received == syscall.SIGINT {
			treeSignal = syscall.SIGINT
		}
	}
	r.writeMeta(func(m *job.Meta) {
		m.TerminationRequestedAt = job.Ptr(job.ISO(requestedAt))
		if reason == stopInterrupted {
			m.InterruptionSignal = job.Ptr(r.term.signal)
		}
	})
	r.progress.Append("stopping",
		job.KV{K: "reason", V: progressReason},
		job.KV{K: "elapsed", V: elapsed(r.startedAt)},
		job.KV{K: "prompt", V: r.meta.PromptState},
	)
	r.escalate(treeSignal)
}

// escalate signals the provider tree and, the first time, schedules SIGKILL:
// a stop already under way keeps the deadline it set.
func (r *run) escalate(sig syscall.Signal) {
	r.signalTree(sig)
	if r.escalation == notStopping {
		r.escalation = terminating
		r.schedule(sigkillAfter, r.kill)
	}
}

// kill sends SIGKILL to the tree and finalizes after one last drain window,
// whatever the streams are still doing.
func (r *run) kill() {
	if r.done || r.escalation == killing {
		return
	}
	r.escalation = killing
	r.signalTree(syscall.SIGKILL)
	r.schedule(closeGrace, func() { r.onChildDone(r.childExit) })
}

// schedule replaces the pending escalation step with fn after d.
func (r *run) schedule(d time.Duration, fn func()) {
	if r.escalationTimer != nil {
		r.escalationTimer.Stop()
	}
	r.escalationTimer = r.afterFunc(d, fn)
}

// cleanupResidual handles a dead child whose group still has members: stop
// them too before publishing terminal state. A stop already under way covers
// them.
func (r *run) cleanupResidual() {
	if r.done || r.escalation != notStopping {
		return
	}
	r.escalate(syscall.SIGTERM)
}

// onExit is process exit — the JS 'exit' event. Direct-child exit is not
// terminal by itself: a grandchild can keep running (or keep pipes open), so
// full completion is judged by maybeStreamsClosed and the fallback timer.
func (r *run) onExit(res exitResult) {
	r.childExit = res
	r.waitDone = true
	if r.term == nil {
		r.exitFallbackTimer = r.afterFunc(closeGrace, func() {
			if r.done {
				return
			}
			if r.groupAlive() {
				r.cleanupResidual()
			} else {
				r.onChildDone(r.childExit)
			}
		})
	}
	r.maybeStreamsClosed()
}

// maybeStreamsClosed is the JS 'close' event: process exited and both stream
// pipes reached EOF (all writers gone).
func (r *run) maybeStreamsClosed() {
	if !r.waitDone || !r.stdoutDone || !r.stderrDone || r.done {
		return
	}
	if r.groupAlive() {
		r.cleanupResidual()
		return
	}
	r.onChildDone(r.childExit)
}

// onChildDone classifies the ended turn and finishes. Branch order matters:
// lock conflict, then requested termination, then unexpected signal, then the
// provider's own conclusion.
func (r *run) onChildDone(exit exitResult) {
	if r.done {
		return
	}
	r.done = true
	for _, t := range []*time.Timer{r.escalationTimer, r.exitFallbackTimer} {
		if t != nil {
			t.Stop()
		}
	}
	r.drainLines(true)

	if r.meta.SessionLockConflict != nil {
		ev := r.driver.Recovery()
		r.finish(provider.Outcome{
			Status: job.StatusInfra,
			ErrorText: fmt.Sprintf("%s reported a session id that another turn already holds, so this turn was stopped.",
				capitalize(r.opts.Provider)),
			Partial:     ev.Partial,
			Tokens:      ev.Tokens,
			PromptState: job.PromptAccepted,
			Evidence:    job.Deref(r.meta.PromptStateEvidence),
		}, exit)
		return
	}

	if r.term != nil && r.meta.ProviderTerminalAt == nil {
		r.finishAfterStop(exit)
		return
	}

	if exit.signal != nil && r.meta.ProviderTerminalAt == nil {
		r.finish(r.driver.Recovery().Outcome(job.StatusInfra, fmt.Sprintf("%s was killed by signal %s, which envoy did not send.",
			r.meta.CommandPrefix[0], *exit.signal)), exit)
		return
	}

	command := ""
	if r.meta.UsesLauncher() {
		command = r.meta.CommandPrefix[0]
	}
	outcome := r.driver.Conclude(provider.ExitInfo{
		Command:      command,
		Code:         exit.code,
		Signal:       exit.signal,
		Terminated:   r.term != nil,
		TerminalType: job.Deref(r.meta.ProviderTerminalEventType),
		StderrTail:   r.stderrTail,
	})
	// The driver reported the cause and any cause-specific fix; the
	// prescription itself is rendered from the records at collect time.
	r.finish(outcome, exit)
}

// finishAfterStop publishes a requested stop (timeout or interruption) with
// whatever acceptance evidence survives. Never redispatch merely because
// output was quiet: that is the recovery invariant these messages encode.
func (r *run) finishAfterStop(exit exitResult) {
	status, stopped := job.StatusInterrupted, fmt.Sprintf("envoy stopped %s after receiving %s.", r.opts.Provider, r.term.signal)
	if r.term.reason == stopTimeout {
		status, stopped = job.StatusTimeout, prose.TimedOut(r.opts.TimeoutMin, r.opts.Provider, r.term.stream)
	}
	r.finish(r.driver.Recovery().Outcome(status, stopped), exit)
}

// capStream reports what the run had observed of the provider's stream as of
// asOf — the instant the caller cares about, which is when the stop was
// requested, not whenever cleanup happens to finish. Every field is read from
// what actually arrived; the engine does not act on any of it, it only says
// it, so the cap stays a deadline rather than a stall detector.
//
// Bytes and events are separate observations because they answer different
// questions: a provider can write to stdout or stderr without producing one
// event envoy can parse, so "no events" never licenses "nothing arrived".
func (r *run) capStream(asOf time.Time) prose.CapStream {
	stream := prose.CapStream{
		Events: r.meta.ProviderEventCount,
		Bytes:  r.meta.ProviderOutputBytes,
	}
	if r.meta.LastProviderEventType != nil {
		stream.LastEvent = *r.meta.LastProviderEventType
	}
	if r.meta.LastProviderActivityAt != nil {
		if t, err := time.Parse(time.RFC3339, *r.meta.LastProviderActivityAt); err == nil {
			if quiet := asOf.Sub(t); quiet > 0 {
				stream.Quiet = quiet
			}
		}
	}
	return stream
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

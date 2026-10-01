package runner

import (
	"fmt"
	"syscall"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
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

// requestTermination starts a stop: TERM the tree, escalate to KILL, then
// give streams one final drain window before publishing terminal metadata.
func (r *run) requestTermination(kind, signalName string) {
	if r.finished || r.childDone {
		return
	}
	if r.term != nil {
		// A second interrupt is an explicit request to stop waiting for cleanup.
		if kind == "interrupted" {
			if r.forceKillTimer != nil {
				r.forceKillTimer.Stop()
			}
			r.forceStopThenFinalize()
		}
		return
	}
	requestedAt := time.Now()
	r.term = &termination{
		kind:        kind,
		signal:      signalName,
		requestedAt: requestedAt,
		stream:      r.capStream(requestedAt),
	}
	r.writeMeta(func(m *job.Meta) {
		m.TerminationRequestedAt = job.Ptr(job.ISO(requestedAt))
		if kind == "interrupted" {
			m.InterruptionSignal = job.Ptr(signalName)
		}
	})
	reason := signalName
	if kind == "timeout" {
		reason = "hard_cap"
	}
	r.progress.Append("stopping",
		job.KV{K: "reason", V: reason},
		job.KV{K: "elapsed", V: elapsed(r.startedAt)},
		job.KV{K: "prompt", V: r.meta.PromptState},
	)
	sig := syscall.SIGTERM
	if signalName == "SIGINT" {
		sig = syscall.SIGINT
	}
	r.signalTree(sig)
	r.forceKillTimer = r.afterFunc(sigkillAfter, r.forceStopThenFinalize)
}

func (r *run) forceStopThenFinalize() {
	if r.forceFinalizeTimer != nil || r.childDone {
		return
	}
	r.signalTree(syscall.SIGKILL)
	r.forceFinalizeTimer = r.afterFunc(closeGrace, func() {
		r.onChildDone(r.childExit)
	})
}

// cleanupResidualThenFinalize handles a dead child whose group still has
// members: stop them too before publishing terminal state.
func (r *run) cleanupResidualThenFinalize() {
	if r.residualCleanup || r.childDone {
		return
	}
	r.residualCleanup = true
	r.signalTree(syscall.SIGTERM)
	r.forceKillTimer = r.afterFunc(sigkillAfter, r.forceStopThenFinalize)
}

// onExit is process exit — the JS 'exit' event. Direct-child exit is not
// terminal by itself: a grandchild can keep running (or keep pipes open), so
// full completion is judged by maybeStreamsClosed and the fallback timer.
func (r *run) onExit(res exitResult) {
	r.childExit = res
	r.waitDone = true
	if r.term == nil {
		r.exitFallbackTimer = r.afterFunc(closeGrace, func() {
			if r.childDone {
				return
			}
			if r.groupAlive() {
				r.cleanupResidualThenFinalize()
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
	if !r.waitDone || !r.stdoutDone || !r.stderrDone || r.childDone {
		return
	}
	if r.groupAlive() {
		if r.term == nil {
			r.cleanupResidualThenFinalize()
		}
		return
	}
	r.onChildDone(r.childExit)
}

// onChildDone classifies the ended turn and finishes. Branch order matters:
// lock conflict, then requested termination, then unexpected signal, then the
// provider's own conclusion.
func (r *run) onChildDone(exit exitResult) {
	if r.childDone {
		return
	}
	r.childDone = true
	for _, t := range []*time.Timer{r.forceKillTimer, r.exitFallbackTimer, r.forceFinalizeTimer} {
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
	var stopped string
	if r.term.kind == "timeout" {
		stopped = prose.TimedOut(r.opts.TimeoutMin, r.opts.Provider, r.term.stream)
	} else {
		sig := r.term.signal
		if sig == "" {
			sig = "an external signal"
		}
		stopped = fmt.Sprintf("envoy stopped %s after receiving %s.", r.opts.Provider, sig)
	}

	status := job.StatusInterrupted
	if r.term.kind == "timeout" {
		status = job.StatusTimeout
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

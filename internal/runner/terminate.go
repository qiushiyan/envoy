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
	r.term = &termination{kind: kind, signal: signalName, requestedAt: requestedAt}
	r.writeMeta(func(m *job.Meta) {
		m.TerminationRequestedAt = job.Ptr(job.ISO(requestedAt))
		if kind == "interrupted" {
			m.InterruptionSignal = job.Ptr(signalName)
		}
		m.NextAction = prose.Stopping()
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

func (r *run) providerTerminalType() string {
	if r.meta.ProviderTerminalEventType == nil {
		return ""
	}
	return *r.meta.ProviderTerminalEventType
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
	r.flushLineBuf()

	if r.conflicted() {
		ev, _ := r.driver.Recovery()
		r.finish(finishArgs{
			status: job.StatusInfra,
			errorText: fmt.Sprintf("%s reported a session id that another turn already holds, so this turn was stopped.",
				capitalize(r.opts.Provider)),
			recovery:            prose.LockedSession(*r.meta.SessionLockConflict, false),
			partial:             ev.Partial,
			tokens:              ev.Tokens,
			promptState:         job.PromptAccepted,
			promptStateEvidence: r.meta.PromptStateEvidence,
			hasEvidence:         true,
			exit:                exit,
		})
		return
	}

	if r.term != nil && r.meta.ProviderTerminalAt == nil {
		r.finishAfterStop(exit)
		return
	}

	if exit.signal != nil && r.meta.ProviderTerminalAt == nil {
		ev, evs := r.driver.Recovery()
		r.handleEvents(evs)
		promptState := job.PromptUnknown
		if ev.Accepted {
			promptState = job.PromptAccepted
		}
		r.finish(finishArgs{
			status: job.StatusInfra,
			errorText: fmt.Sprintf("%s was killed by signal %s, which envoy did not send.",
				r.opts.Provider, *exit.signal),
			recovery:            prose.Recovery(promptState, r.resumeCommand(), ""),
			partial:             ev.Partial,
			tokens:              ev.Tokens,
			costUSD:             ev.CostUSD,
			promptState:         promptState,
			promptStateEvidence: ptrIfNonEmpty(ev.Label),
			hasEvidence:         true,
			exit:                exit,
		})
		return
	}

	outcome := r.driver.Conclude(provider.ExitInfo{
		Code:         exit.code,
		Signal:       exit.signal,
		Terminated:   r.term != nil,
		TerminalType: r.providerTerminalType(),
		StderrTail:   r.stderrTail,
	})
	if outcome.SessionID != "" {
		r.setSession(outcome.SessionID)
	}
	// The driver reported the cause; the prescription follows from the prompt
	// state it observed, worded once in prose.
	recovery := ""
	if outcome.Status != job.StatusOK {
		promptState := outcome.PromptState
		if promptState == "" {
			promptState = r.meta.PromptState
		}
		recovery = prose.Recovery(promptState, r.resumeCommand(), outcome.Remedy)
	}
	r.finish(finishArgs{
		status:              outcome.Status,
		text:                outcome.Text,
		errorText:           outcome.ErrorText,
		recovery:            recovery,
		partial:             outcome.Partial,
		tokens:              outcome.Tokens,
		costUSD:             outcome.CostUSD,
		promptState:         outcome.PromptState,
		promptStateEvidence: outcome.PromptStateEvidence,
		hasEvidence:         outcome.HasEvidence,
		exit:                exit,
	})
}

// finishAfterStop publishes a requested stop (timeout or interruption) with
// whatever acceptance evidence survives. Never redispatch merely because
// output was quiet: that is the recovery invariant these messages encode.
func (r *run) finishAfterStop(exit exitResult) {
	ev, evs := r.driver.Recovery()
	r.handleEvents(evs)

	var stopped string
	if r.term.kind == "timeout" {
		stopped = fmt.Sprintf(
			"The %g-minute wall-clock cap ended this %s turn. The cap counts healthy work too, so reaching it is not evidence the provider hung.",
			r.opts.Turn.TimeoutMin, r.opts.Provider)
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
	promptState := job.PromptUnknown
	var evidence *string
	if ev.Accepted {
		promptState = job.PromptAccepted
		evidence = ptrIfNonEmpty(ev.Label)
	}
	r.finish(finishArgs{
		status:              status,
		errorText:           stopped,
		recovery:            prose.Recovery(promptState, r.resumeCommand(), ""),
		partial:             ev.Partial,
		tokens:              ev.Tokens,
		costUSD:             ev.CostUSD,
		promptState:         promptState,
		promptStateEvidence: evidence,
		hasEvidence:         true,
		exit:                exit,
	})
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

func ptrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return job.Ptr(s)
}

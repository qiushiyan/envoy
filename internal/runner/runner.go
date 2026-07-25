// Package runner supervises one provider turn: spawn in an own process group,
// pump the streams, keep the deadline and heartbeat, stop the whole tree on
// timeout or interruption, and publish terminal state as durable files.
//
// Everything runs on one event-loop goroutine; helper goroutines (readers,
// process wait, timers) only send into its channels. That keeps the state
// machine as race-free as the single-threaded original.
package runner

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/lock"
	"github.com/qiushiyan/envoy/internal/provider"
	"github.com/qiushiyan/envoy/internal/text"
)

// Tunables, env-overridable so the integration suite can run the full
// lifecycle in seconds.
var (
	heartbeatEvery = durationFromEnv("ENVOY_HEARTBEAT_MS", 30_000*time.Millisecond)
	timeoutPoll    = durationFromEnv("ENVOY_TIMEOUT_POLL_MS", 5_000*time.Millisecond)
	sigkillAfter   = durationFromEnv("ENVOY_SIGKILL_AFTER_MS", 10_000*time.Millisecond)
	closeGrace     = durationFromEnv("ENVOY_CLOSE_GRACE_MS", 2_000*time.Millisecond)
)

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	ms, err := strconv.ParseFloat(raw, 64)
	if err != nil || ms < 0 {
		return fallback
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// Options is one validated turn request. The CLI owns flag parsing and usage
// errors; the runner owns everything after.
type Options struct {
	Spec   provider.Spec
	OutDir string // "" = derive from cwd/label
	Stdout io.Writer
	Stderr io.Writer
}

type termination struct {
	kind        string // "timeout" | "interrupted" | "lock_conflict"
	signal      string
	requestedAt time.Time
}

type exitResult struct {
	code   *int
	signal *string
}

type run struct {
	opts      Options
	spec      provider.Spec
	driver    provider.Driver
	ws        job.Workspace
	progress  *job.ProgressLog
	meta      *job.Meta
	startedAt time.Time
	deadline  time.Time // zero = no cap
	instance  string

	sessionID   string
	sessionLock *lock.Handle
	lockErr     string // agent-facing conflict message, "" = none

	rawFile    *os.File
	stderrFile *os.File

	child      *childProcess
	stderrTail string
	lineBuf    []byte

	// observation counters (meta + heartbeat)
	outputBytes    int64
	eventCount     int64
	lastOutputAt   *string
	lastActivityAt *string
	lastEventType  *string

	providerTerminalAt   string
	providerTerminalType string

	term               *termination
	waitDone           bool
	stdoutDone         bool
	stderrDone         bool
	childExit          exitResult
	childDone          bool
	residualCleanup    bool
	forceKillTimer     *time.Timer
	exitFallbackTimer  *time.Timer
	forceFinalizeTimer *time.Timer
	finished           bool
	exitCode           int

	callCh   chan func()
	stdoutCh chan []byte
	stderrCh chan []byte
	waitCh   chan exitResult
	sigCh    chan os.Signal
}

// Run executes one turn and returns the process exit code.
func Run(opts Options) int {
	startedAt := time.Now()
	r := &run{
		opts:      opts,
		spec:      opts.Spec,
		startedAt: startedAt,
		instance:  job.UUID4(),
		callCh:    make(chan func(), 32),
		stdoutCh:  make(chan []byte, 32),
		stderrCh:  make(chan []byte, 32),
		waitCh:    make(chan exitResult, 1),
		sigCh:     make(chan os.Signal, 4),
	}
	if r.spec.TimeoutMin > 0 {
		r.deadline = startedAt.Add(time.Duration(r.spec.TimeoutMin * float64(time.Minute)))
	}

	outDir, err := job.ResolveOutDir(opts.OutDir, r.spec.Cwd, r.spec.Label, r.spec.Provider, startedAt)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "envoy: cannot create out-dir: %s\n", err)
		return job.ExitInfra
	}
	r.ws = job.Workspace{Dir: outDir}
	r.progress = r.ws.Progress()

	driver, err := provider.New(r.spec, r.ws, startedAt)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "usage error: %s\n", err)
		return job.ExitUsage
	}
	r.driver = driver

	promptText, err := os.ReadFile(r.spec.PromptFile)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "envoy: cannot read prompt file: %s\n", err)
		return job.ExitInfra
	}

	// A known session id (claude, or any --resume) locks before any job
	// artifact is written, so a rejected racing resume cannot truncate the
	// live job's files.
	r.sessionID = driver.PreflightSessionID()
	if r.sessionID != "" {
		handle, err := lock.Acquire(r.sessionID, outDir, r.instance)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "lock error: %s\n", err)
			if _, ok := err.(*lock.Conflict); ok {
				return job.ExitUsage
			}
			return job.ExitInfra
		}
		r.sessionLock = handle
	}

	if err := r.ws.Prepare(r.spec.PromptFile); err != nil {
		r.sessionLock.Release()
		fmt.Fprintf(opts.Stderr, "envoy: cannot prepare job dir: %s\n", err)
		return job.ExitInfra
	}
	return r.execute(string(promptText))
}

func (r *run) execute(promptText string) int {
	r.initMeta()
	r.printStartupBlock()
	r.progress.Append("starting",
		job.KV{K: "provider", V: r.spec.Provider},
		job.KV{K: "hard_cap", V: text.HardCap(r.spec.TimeoutMin)},
		job.KV{K: "prompt", V: job.PromptUnknown},
	)
	r.writeMeta(nil)

	rawFile, err := os.OpenFile(r.ws.RawLogPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		r.rawFile = rawFile
		defer rawFile.Close()
	}
	stderrFile, err := os.OpenFile(r.ws.StderrLogPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		r.stderrFile = stderrFile
		defer stderrFile.Close()
	}

	child, err := spawn(r.spec.Provider, r.driver.Argv(r.ws), r.spec.Cwd, r.driver.ExtraEnv())
	if err != nil {
		r.finish(finishArgs{
			status:              job.StatusInfra,
			errorText:           fmt.Sprintf("The envoy runtime could not start %s: %s", r.spec.Provider, err),
			nextAction:          "The prompt was not accepted. Retry the identical dispatch once; if startup fails again, check the provider executable and report the infrastructure failure.",
			promptState:         job.PromptNotStarted,
			promptStateEvidence: job.Ptr("provider spawn error"),
			hasEvidence:         true,
		})
		return r.exitCode
	}
	r.child = child
	r.writeMeta(func(m *job.Meta) {
		m.ProviderPid = job.Ptr(child.pid)
		m.ProviderPgid = job.Ptr(child.pid) // detached child leads its own group
	})
	r.progress.Append("running",
		job.KV{K: "provider_process", V: "alive"},
		job.KV{K: "provider_pid", V: child.pid},
		job.KV{K: "prompt", V: r.meta.PromptState},
	)

	go child.pump(child.stdout, r.stdoutCh)
	go child.pump(child.stderr, r.stderrCh)
	go func() { r.waitCh <- child.wait() }()
	go func() {
		// codex exec blocks forever on an open stdin pipe; close after the
		// prompt no matter what.
		_, werr := child.stdin.Write([]byte(promptText))
		if cerr := child.stdin.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			msg := werr.Error()
			r.callCh <- func() {
				r.progress.Append("input-error", job.KV{K: "detail", V: msg})
			}
		}
	}()

	signal.Notify(r.sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(r.sigCh)

	var heartbeatC, timeoutC <-chan time.Time
	if heartbeatEvery > 0 {
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		heartbeatC = t.C
	}
	if !r.deadline.IsZero() {
		poll := timeoutPoll
		if poll < time.Millisecond {
			poll = time.Millisecond
		}
		t := time.NewTicker(poll)
		defer t.Stop()
		timeoutC = t.C
	}

	stdoutCh, stderrCh := r.stdoutCh, r.stderrCh
	for !r.finished {
		select {
		case fn := <-r.callCh:
			fn()
		case chunk, ok := <-stdoutCh:
			if !ok {
				stdoutCh = nil
				r.stdoutDone = true
				r.maybeStreamsClosed()
				continue
			}
			r.onStdout(chunk)
		case chunk, ok := <-stderrCh:
			if !ok {
				stderrCh = nil
				r.stderrDone = true
				r.maybeStreamsClosed()
				continue
			}
			r.onStderr(chunk)
		case res := <-r.waitCh:
			r.onExit(res)
		case sig := <-r.sigCh:
			r.requestTermination("interrupted", sigName(sig))
		case <-heartbeatC:
			r.heartbeat()
		case <-timeoutC:
			if !time.Now().Before(r.deadline) {
				timeoutC = nil
				r.requestTermination("timeout", "SIGTERM")
			}
		}
	}
	return r.exitCode
}

// ---------- stream handling ----------

func (r *run) onStdout(chunk []byte) {
	if r.rawFile != nil {
		r.rawFile.Write(chunk)
	}
	r.noteOutput(len(chunk))
	r.lineBuf = append(r.lineBuf, chunk...)
	r.drainLines(false)
}

// drainLines feeds every complete line to the driver; final also flushes an
// unterminated tail (a final JSON record without a newline still counts).
func (r *run) drainLines(final bool) {
	for {
		i := bytes.IndexByte(r.lineBuf, '\n')
		if i < 0 {
			break
		}
		line := string(r.lineBuf[:i])
		r.lineBuf = r.lineBuf[i+1:]
		r.handleEvents(r.driver.Feed(line))
	}
	if final && len(r.lineBuf) > 0 {
		line := string(r.lineBuf)
		r.lineBuf = nil
		r.handleEvents(r.driver.Feed(line))
	}
}

func (r *run) flushLineBuf() { r.drainLines(true) }

func (r *run) onStderr(chunk []byte) {
	if r.stderrFile != nil {
		r.stderrFile.Write(chunk)
	}
	r.noteOutput(len(chunk))
	r.stderrTail = tail(r.stderrTail+string(chunk), 2000)
}

func (r *run) noteOutput(n int) {
	r.outputBytes += int64(n)
	r.lastOutputAt = job.Ptr(job.ISO(time.Now()))
}

// handleEvents applies a driver's semantic events. A session-lock conflict
// drops the remaining events of that batch (the acceptance that would have
// followed the session id is recorded as a conflict instead), but later
// stream lines still feed the driver's accumulators.
func (r *run) handleEvents(events []provider.Event) {
	for i, ev := range events {
		switch ev.Kind {
		case provider.KindActivity:
			r.eventCount++
			r.lastActivityAt = job.Ptr(job.ISO(time.Now()))
			r.lastEventType = job.Ptr(ev.Type)
		case provider.KindNote:
			r.progress.Append(ev.State, ev.Fields...)
		case provider.KindAccepted:
			r.markPromptAccepted(ev.Evidence)
		case provider.KindTerminal:
			r.markProviderTerminal(ev.Terminal)
		case provider.KindSessionStarted:
			if r.onSessionStarted(ev, events[i+1:]) {
				return
			}
		}
	}
}

// onSessionStarted handles a fresh mid-stream session id (a fresh codex
// thread). A collision with an existing lock is improbable, but once observed
// this turn must stop rather than continue unlocked.
func (r *run) onSessionStarted(ev provider.Event, rest []provider.Event) (abort bool) {
	r.sessionID = ev.SessionID
	fmt.Fprintf(r.opts.Stdout, "session: %s\n", r.sessionID)
	if r.sessionLock == nil {
		handle, err := lock.Acquire(r.sessionID, r.ws.Dir, r.instance)
		if conflict, ok := err.(*lock.Conflict); ok {
			r.lockErr = conflict.Message
			evidence := r.spec.Provider + " session started"
			for _, e := range rest {
				if e.Kind == provider.KindAccepted {
					evidence = e.Evidence
					break
				}
			}
			r.markPromptAccepted(evidence + " with conflicting session lock")
			fmt.Fprintf(r.opts.Stderr, "lock error: %s\n", r.lockErr)
			r.progress.Append("lock-conflict",
				job.KV{K: "session", V: r.sessionID},
				job.KV{K: "action", V: "stopping"},
			)
			r.requestTermination("lock_conflict", "SIGTERM")
			return true
		}
		if err != nil {
			// Treat unexpected lock infrastructure failure like a conflict:
			// never continue an unlocked session.
			r.lockErr = err.Error()
			r.requestTermination("lock_conflict", "SIGTERM")
			return true
		}
		r.sessionLock = handle
	}
	fmt.Fprintf(r.opts.Stdout, "takeover-after-terminal: %s\n", r.driver.Takeover())
	return false
}

func (r *run) markPromptAccepted(evidence string) {
	if r.meta.PromptState == job.PromptAccepted {
		return
	}
	acceptedAt := job.ISO(time.Now())
	r.writeMeta(func(m *job.Meta) {
		m.PromptState = job.PromptAccepted
		m.PromptStateEvidence = job.Ptr(evidence)
		m.PromptAcceptedAt = job.Ptr(acceptedAt)
	})
	session := r.sessionID
	if session == "" {
		session = "pending"
	}
	r.progress.Append("accepted",
		job.KV{K: "prompt", V: "accepted"},
		job.KV{K: "evidence", V: evidence},
		job.KV{K: "session", V: session},
	)
}

func (r *run) markProviderTerminal(label string) {
	if r.providerTerminalAt != "" {
		return
	}
	r.providerTerminalAt = job.ISO(time.Now())
	r.providerTerminalType = label
	r.writeMeta(func(m *job.Meta) {
		m.ProviderTerminalAt = job.Ptr(r.providerTerminalAt)
		m.ProviderTerminalEventType = job.Ptr(label)
	})
	r.progress.Append("provider-terminal",
		job.KV{K: "event", V: label},
		job.KV{K: "prompt", V: r.meta.PromptState},
	)
}

func (r *run) heartbeat() {
	r.handleEvents(r.driver.Poll())
	now := time.Now()
	activityAge := "none"
	if r.lastActivityAt != nil {
		if t, err := time.Parse(time.RFC3339, *r.lastActivityAt); err == nil {
			activityAge = text.FormatDuration(now.Sub(t)) + "_ago"
		}
	}
	providerAlive := "not_observed"
	if r.groupAlive() {
		providerAlive = "alive"
	}
	r.writeMeta(func(m *job.Meta) {
		m.LastHeartbeatAt = job.Ptr(job.ISO(now))
	})
	r.progress.Append("running",
		job.KV{K: "elapsed", V: text.FormatDuration(now.Sub(r.startedAt))},
		job.KV{K: "provider_process", V: providerAlive},
		job.KV{K: "last_provider_activity", V: activityAge},
		job.KV{K: "prompt", V: r.meta.PromptState},
		job.KV{K: "events", V: r.eventCount},
	)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

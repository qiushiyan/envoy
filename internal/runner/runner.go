// Package runner supervises one provider turn: spawn in an own process group,
// pump the streams, keep the deadline and heartbeat, stop the whole tree on
// timeout or interruption, and publish terminal state as durable files.
//
// Everything runs on one event-loop goroutine; helper goroutines (readers,
// process wait, timers) only send into its channels. That keeps the state
// machine as race-free as the single-threaded original. The in-memory
// job.Meta is the single copy of published state: the loop mutates it
// directly and writeMeta snapshots it to disk — there is deliberately no
// second, runner-private copy of anything meta records.
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
	"github.com/qiushiyan/envoy/internal/prose"
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
	Provider   string
	PromptFile string
	Cwd        string
	Baseline   string // "" = HEAD for write turns, else unset
	Label      string
	OutDir     string // "" = derive from cwd/label
	Turn       provider.Options
	// SessionLock is a lock a supervisor already holds for this turn's
	// resumed session — a fan-out reserves every member before any spawns,
	// so a held session refuses the whole round instead of one member. The
	// runner takes ownership and releases it however the turn ends.
	SessionLock *lock.Handle
	Stdout      io.Writer
	Stderr      io.Writer
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
	driver    provider.Driver
	ws        job.Workspace
	progress  *job.ProgressLog
	meta      *job.Meta // single in-memory copy of published state
	startedAt time.Time
	deadline  time.Time // zero = no cap; wall clock, never monotonic
	instance  string
	argv      []string // provider argv, computed once: spawned and recorded

	sessionLock *lock.Handle

	rawFile    *os.File
	stderrFile *os.File

	child      *childProcess
	stderrTail string
	lineBuf    []byte

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

// deadlineFrom computes the hard cap as a pure wall-clock instant. Round(0)
// strips the monotonic reading: Go compares two monotonic-bearing times on
// the monotonic clock, which freezes during laptop sleep — the cap must not
// stretch across a suspend, so the first post-wake poll has to catch the
// overrun on the wall clock.
func deadlineFrom(startedAt time.Time, timeoutMin float64) time.Time {
	if timeoutMin <= 0 {
		return time.Time{}
	}
	return startedAt.Round(0).Add(time.Duration(timeoutMin * float64(time.Minute)))
}

// Result is one turn's outcome for whoever started it. Status is empty when
// the turn ended before meta.json existed — a rejected flag, a locked session,
// an unreadable prompt — which a supervisor must be able to tell apart from a
// turn that ran and published a terminal status.
type Result struct {
	ExitCode int
	OutDir   string
	Status   string
}

// Run executes one turn and returns its outcome.
func Run(opts Options) Result {
	// Round(0) keeps every derived duration and timestamp on the wall clock.
	startedAt := time.Now().Round(0)
	r := &run{
		opts:      opts,
		startedAt: startedAt,
		deadline:  deadlineFrom(startedAt, opts.Turn.TimeoutMin),
		instance:  job.UUID4(),
		callCh:    make(chan func(), 32),
		stdoutCh:  make(chan []byte, 32),
		stderrCh:  make(chan []byte, 32),
		waitCh:    make(chan exitResult, 1),
		sigCh:     make(chan os.Signal, 4),
	}

	// A supervisor's reservation is owned from the first moment, so every
	// early return below releases it instead of stranding the session.
	r.sessionLock = opts.SessionLock

	outDir, err := job.ResolveOutDir(opts.OutDir, opts.Cwd, opts.Label, opts.Provider, startedAt)
	if err != nil {
		r.releaseLock()
		fmt.Fprintf(opts.Stderr, "envoy: cannot create out-dir: %s\n", err)
		return Result{ExitCode: job.ExitInfra, OutDir: opts.OutDir}
	}
	r.ws = job.Workspace{Dir: outDir}
	r.progress = r.ws.Progress()
	r.progress.Warn = opts.Stderr

	driver, err := provider.New(opts.Provider, opts.Turn, r.ws, startedAt)
	if err != nil {
		r.releaseLock()
		fmt.Fprintf(opts.Stderr, "usage error: %s\n", err)
		return Result{ExitCode: job.ExitUsage, OutDir: outDir}
	}
	r.driver = driver

	promptText, err := os.ReadFile(opts.PromptFile)
	if err != nil {
		r.releaseLock()
		fmt.Fprintf(opts.Stderr, "envoy: cannot read prompt file: %s\n", err)
		return Result{ExitCode: job.ExitInfra, OutDir: outDir}
	}

	// A known session id (claude, or any --resume) locks before any job
	// artifact is written, so a rejected racing resume cannot truncate the
	// live job's files. A supervisor may have reserved it already.
	if sessionID := driver.PreflightSessionID(); sessionID != "" && r.sessionLock == nil {
		handle, err := lock.Acquire(sessionID, outDir, r.instance)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "lock error: %s\n", err)
			if _, ok := err.(*lock.Conflict); ok {
				return Result{ExitCode: job.ExitUsage, OutDir: outDir}
			}
			return Result{ExitCode: job.ExitInfra, OutDir: outDir}
		}
		r.sessionLock = handle
	}

	if err := r.ws.Prepare(opts.PromptFile); err != nil {
		r.releaseLock()
		fmt.Fprintf(opts.Stderr, "envoy: cannot prepare job dir: %s\n", err)
		return Result{ExitCode: job.ExitInfra, OutDir: outDir}
	}
	return r.execute(string(promptText))
}

func (r *run) execute(promptText string) Result {
	r.initMeta()
	r.printStartupBlock()
	r.progress.Append("starting",
		job.KV{K: "provider", V: r.opts.Provider},
		job.KV{K: "hard_cap", V: text.HardCap(r.opts.Turn.TimeoutMin)},
		job.KV{K: "prompt", V: job.PromptUnknown},
	)
	r.writeMeta(nil)

	// raw.log and stderr.log are contract artifacts: failing to open one
	// must be visible, even though the turn itself can proceed.
	rawFile, err := os.OpenFile(r.ws.RawLogPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(r.opts.Stderr, "raw log warning: provider stdout will not be recorded: %s\n", err)
	} else {
		r.rawFile = rawFile
		defer rawFile.Close()
	}
	stderrFile, err := os.OpenFile(r.ws.StderrLogPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(r.opts.Stderr, "stderr log warning: provider stderr will not be recorded: %s\n", err)
	} else {
		r.stderrFile = stderrFile
		defer stderrFile.Close()
	}

	child, err := spawn(r.opts.Provider, r.argv, r.opts.Cwd, r.driver.ExtraEnv())
	if err != nil {
		r.finish(finishArgs{
			status:              job.StatusInfra,
			errorText:           prose.SpawnFailed(r.opts.Provider, err),
			recovery:            prose.Recovery(job.PromptNotStarted, r.resumeCommand(), ""),
			promptState:         job.PromptNotStarted,
			promptStateEvidence: job.Ptr("provider spawn error"),
			hasEvidence:         true,
		})
		return r.result()
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
	return r.result()
}

// result reports the turn as its caller sees it, reading the same in-memory
// meta that was published to disk.
func (r *run) result() Result {
	return Result{ExitCode: r.exitCode, OutDir: r.ws.Dir, Status: r.meta.Status}
}

// ---------- session state (the one owner of the coordinate invariant) ----------

func (r *run) session() string {
	if r.meta.SessionID == nil {
		return ""
	}
	return *r.meta.SessionID
}

func (r *run) conflicted() bool { return r.meta.SessionLockConflict != nil }

// resumeCommand is the follow-up command for this turn's session, carrying
// the settings it was dispatched with.
func (r *run) resumeCommand() string {
	return prose.Turn{
		Provider:   r.opts.Provider,
		SessionID:  r.session(),
		Cwd:        r.opts.Cwd,
		Model:      r.opts.Turn.Model,
		Effort:     r.opts.Turn.Effort,
		AllowWrite: r.opts.Turn.AllowWrite,
		TimeoutMin: r.opts.Turn.TimeoutMin,
	}.ResumeCommand()
}

// setSession records the session id and refreshes its derived coordinates.
// The invariant "resume/takeover present iff a session id exists and no lock
// conflict was recorded" lives here and in markLockConflict — nowhere else.
func (r *run) setSession(id string) {
	r.meta.SessionID = ptrIfNonEmpty(id)
	r.syncSessionCoords()
}

func (r *run) markLockConflict(msg string) {
	r.meta.SessionLockConflict = job.Ptr(msg)
	r.syncSessionCoords()
}

func (r *run) syncSessionCoords() {
	if r.session() != "" && !r.conflicted() {
		r.meta.ResumeCommand = ptrIfNonEmpty(r.resumeCommand())
		r.meta.TakeoverCommand = ptrIfNonEmpty(r.driver.Takeover())
		return
	}
	r.meta.ResumeCommand = nil
	r.meta.TakeoverCommand = nil
}

func (r *run) releaseLock() {
	if err := r.sessionLock.Release(); err != nil {
		fmt.Fprintf(r.opts.Stderr, "lock cleanup warning: %s\n", err)
	}
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
	r.meta.ProviderOutputBytes += int64(n)
	r.meta.LastProviderOutputAt = job.Ptr(job.ISO(time.Now()))
}

// handleEvents applies a driver's semantic events. A session-lock conflict
// drops the remaining events of that batch (the acceptance that would have
// followed the session id is recorded as a conflict instead), but later
// stream lines still feed the driver's accumulators.
func (r *run) handleEvents(events []provider.Event) {
	for i, ev := range events {
		switch ev.Kind {
		case provider.KindActivity:
			r.meta.ProviderEventCount++
			r.meta.LastProviderActivityAt = job.Ptr(job.ISO(time.Now()))
			r.meta.LastProviderEventType = job.Ptr(ev.Type)
		case provider.KindNote:
			r.progress.Append(ev.State, ev.Fields...)
		case provider.KindModelReported:
			// The provider's own statement of what it resolved — recorded as
			// an observation, never inferred.
			model := ev.Model
			r.writeMeta(func(m *job.Meta) { m.ProviderReportedModel = job.Ptr(model) })
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
	r.setSession(ev.SessionID)
	fmt.Fprintf(r.opts.Stdout, "session: %s\n", ev.SessionID)
	if r.sessionLock == nil {
		handle, err := lock.Acquire(ev.SessionID, r.ws.Dir, r.instance)
		if err != nil {
			// A non-conflict lock failure is treated the same way: never
			// continue an unlocked session.
			r.markLockConflict(err.Error())
			evidence := r.opts.Provider + " session started"
			for _, e := range rest {
				if e.Kind == provider.KindAccepted {
					evidence = e.Evidence
					break
				}
			}
			r.markPromptAccepted(evidence + " with conflicting session lock")
			fmt.Fprintf(r.opts.Stderr, "lock error: %s\n", err)
			r.progress.Append("lock-conflict",
				job.KV{K: "session", V: ev.SessionID},
				job.KV{K: "action", V: "stopping"},
			)
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
	session := r.session()
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
	if r.meta.ProviderTerminalAt != nil {
		return
	}
	r.writeMeta(func(m *job.Meta) {
		m.ProviderTerminalAt = job.Ptr(job.ISO(time.Now()))
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
	if r.meta.LastProviderActivityAt != nil {
		if t, err := time.Parse(time.RFC3339, *r.meta.LastProviderActivityAt); err == nil {
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
		job.KV{K: "events", V: r.meta.ProviderEventCount},
	)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

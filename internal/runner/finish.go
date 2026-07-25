package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/gitx"
	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/text"
)

func (r *run) initMeta() {
	baseline := r.spec.Baseline
	if baseline == "" && r.spec.AllowWrite {
		// The review anchor: a write turn defaults to HEAD so collect can
		// always diff the delegate's work.
		baseline = gitx.Head(r.spec.Cwd)
	}
	promptFile, err := filepath.Abs(r.spec.PromptFile)
	if err != nil {
		promptFile = r.spec.PromptFile
	}
	var deadlineAt *string
	if !r.deadline.IsZero() {
		deadlineAt = job.Ptr(job.ISO(r.deadline))
	}
	r.meta = &job.Meta{
		SchemaVersion:    job.MetaSchemaVersion,
		Status:           job.StatusRunning,
		Provider:         r.spec.Provider,
		Model:            ptrIfNonEmpty(r.spec.Model),
		Effort:           ptrIfNonEmpty(r.spec.Effort),
		Cwd:              r.spec.Cwd,
		AllowWrite:       r.spec.AllowWrite,
		GitBaseline:      ptrIfNonEmpty(baseline),
		StartedAt:        job.ISO(r.startedAt),
		TimeoutMin:       r.spec.TimeoutMin,
		DeadlineAt:       deadlineAt,
		Label:            ptrIfNonEmpty(r.spec.Label),
		PromptFile:       promptFile,
		OutDir:           r.ws.Dir,
		RawPath:          r.ws.RawLogPath(),
		StderrPath:       r.ws.StderrLogPath(),
		ProgressPath:     r.ws.ProgressLogPath(),
		WatchCommand:     r.ws.WatchCommand(),
		ProviderArgv:     append([]string{r.spec.Provider}, r.driver.Argv(r.ws)...),
		RunnerPid:        os.Getpid(),
		RunnerInstanceID: r.instance,
		PromptState:      job.PromptUnknown,
		ResultKind:       job.ResultNone,
		NextAction:       "Return now and wait for Claude Code's native background-task notification. Use the watch command only for live observation; it is not a completion signal.",
	}
}

// writeMeta atomically replaces meta.json with the current state plus the
// caller's mutation. Session coordinates and observation counters are always
// refreshed so every snapshot is internally consistent.
func (r *run) writeMeta(mutate func(*job.Meta)) {
	m := r.meta
	sessionAvailable := r.sessionID != "" && r.lockErr == ""
	m.SessionID = ptrIfNonEmpty(r.sessionID)
	if sessionAvailable {
		m.ResumeFlag = job.Ptr("--resume " + r.sessionID)
		m.ResumeArgs = ptrIfNonEmpty(r.driver.ResumeArgs())
		m.TakeoverCommand = ptrIfNonEmpty(r.driver.Takeover())
	} else {
		m.ResumeFlag = nil
		m.ResumeArgs = nil
		m.TakeoverCommand = nil
	}
	m.SessionLockConflict = ptrIfNonEmpty(r.lockErr)
	m.ProviderOutputBytes = r.outputBytes
	m.ProviderEventCount = r.eventCount
	m.LastProviderOutputAt = r.lastOutputAt
	m.LastProviderActivityAt = r.lastActivityAt
	m.LastProviderEventType = r.lastEventType
	m.ProviderReportedModel = ptrIfNonEmpty(r.reportedModel)
	if mutate != nil {
		mutate(m)
	}
	if err := m.WriteFile(r.ws.MetaPath()); err != nil {
		fmt.Fprintf(r.opts.Stderr, "meta write warning: %s\n", err)
	}
}

func (r *run) printStartupBlock() {
	w := r.opts.Stdout
	display := func(v string) string {
		if v == "" {
			return "(provider default)"
		}
		return v
	}
	fmt.Fprintf(w, "out-dir: %s\n", r.ws.Dir)
	fmt.Fprintf(w, "provider: %s · model %s · effort %s · hard cap %s\n",
		r.spec.Provider, display(r.spec.Model), display(r.spec.Effort), text.HardCap(r.spec.TimeoutMin))
	fmt.Fprintf(w, "watch: %s\n", r.ws.WatchCommand())
	fmt.Fprintf(w, "raw: %s\n", r.ws.RawLogPath())
	fmt.Fprintf(w, "stderr: %s\n", r.ws.StderrLogPath())
	if r.meta.GitBaseline != nil {
		fmt.Fprintf(w, "baseline: %s\n", *r.meta.GitBaseline)
	}
	if r.sessionID != "" {
		fmt.Fprintf(w, "session: %s\n", r.sessionID)
		fmt.Fprintf(w, "takeover-after-terminal: %s\n", r.driver.Takeover())
	}
	fmt.Fprintln(w, "next: return now; wait for the native background-task notification, then collect this job")
}

type finishArgs struct {
	status              string
	text                string // final text, ok only
	errorText           string
	nextAction          string
	partial             *string
	tokens              *job.Tokens
	costUSD             *float64
	promptState         string // "" = keep current
	promptStateEvidence *string
	hasEvidence         bool
	exit                exitResult
}

// finish publishes terminal state: result.md, final meta, the terminal
// coordinate block, and the exit code. Idempotent; first caller wins.
func (r *run) finish(f finishArgs) {
	if r.finished {
		return
	}
	r.finished = true
	for _, t := range []*time.Timer{r.forceKillTimer, r.exitFallbackTimer, r.forceFinalizeTimer} {
		if t != nil {
			t.Stop()
		}
	}
	endedAt := time.Now()
	collectAction := fmt.Sprintf("Collect and verify this job: envoy collect %s.", text.ShellQuote(r.ws.Dir))

	var recoveryAction *string
	if f.status != job.StatusOK {
		if f.nextAction != "" {
			recoveryAction = job.Ptr(f.nextAction)
		} else {
			recoveryAction = job.Ptr("Inspect result.md, progress.log, raw.log, and stderr.log before choosing a recovery action.")
		}
	}

	hasPartial := f.partial != nil && strings.TrimSpace(*f.partial) != ""
	resultKind := job.ResultNone
	var resultBody string
	if f.status == job.StatusOK {
		resultKind = job.ResultFinal
		resultBody = f.text
	} else {
		resultBody = fmt.Sprintf("# Turn %s\n\n%s\n", f.status, f.errorText)
		if hasPartial {
			resultKind = job.ResultPartial
			resultBody += fmt.Sprintf("\n## Partial output recovered before the failure\n\n%s\n", *f.partial)
		}
	}
	if err := job.WriteFileAtomic(r.ws.ResultPath(), []byte(resultBody)); err != nil {
		fmt.Fprintf(r.opts.Stderr, "result write warning: %s\n", err)
	}

	r.writeMeta(func(m *job.Meta) {
		m.Status = f.status
		m.EndedAt = job.Ptr(job.ISO(endedAt))
		m.DurationMs = job.Ptr(endedAt.Sub(r.startedAt).Milliseconds())
		m.Tokens = f.tokens
		m.CostUSD = f.costUSD
		if f.status == job.StatusOK {
			m.Error = nil
		} else {
			m.Error = ptrIfNonEmpty(f.errorText)
		}
		m.NextAction = collectAction
		m.RecoveryAction = recoveryAction
		if f.promptState != "" {
			m.PromptState = f.promptState
		}
		if f.hasEvidence {
			m.PromptStateEvidence = f.promptStateEvidence
		}
		m.ResultKind = resultKind
		m.ChildExitCode = f.exit.code
		m.ChildExitSignal = f.exit.signal
		m.CollectedAt = nil
	})
	r.progress.Append("terminal",
		job.KV{K: "status", V: f.status},
		job.KV{K: "elapsed", V: elapsed(r.startedAt)},
		job.KV{K: "result", V: resultKind},
		job.KV{K: "prompt", V: r.meta.PromptState},
	)
	r.sessionLock.Release()

	w := r.opts.Stdout
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "status: %s\n", f.status)
	fmt.Fprintf(w, "result: %s\n", r.ws.ResultPath())
	fmt.Fprintf(w, "meta: %s\n", r.ws.MetaPath())
	session := r.sessionID
	if session == "" {
		session = "(none)"
	}
	fmt.Fprintf(w, "session: %s\n", session)
	if r.sessionID != "" && r.lockErr == "" {
		fmt.Fprintf(w, "takeover: %s\n", r.driver.Takeover())
	}
	fmt.Fprintf(w, "next: %s\n", collectAction)

	r.exitCode = job.ExitCodeFor(f.status)
}

func elapsed(since time.Time) string {
	return text.FormatDuration(time.Since(since))
}

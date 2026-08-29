package runner

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/gitx"
	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
	"github.com/qiushiyan/envoy/internal/text"
)

func (r *run) initMeta() {
	baseline := r.opts.Baseline
	if baseline == "" && r.opts.Turn.AllowWrite {
		// The review anchor: a write turn defaults to HEAD so collect can
		// always diff the delegate's work.
		baseline = gitx.Head(r.opts.Cwd)
	}
	promptFile, err := filepath.Abs(r.opts.PromptFile)
	if err != nil {
		promptFile = r.opts.PromptFile
	}
	var deadlineAt *string
	if !r.deadline.IsZero() {
		deadlineAt = job.Ptr(job.ISO(r.deadline))
	}
	// The argv is computed exactly once: what meta records is what spawns.
	r.argv = r.driver.Argv()
	r.meta = &job.Meta{
		SchemaVersion:    job.MetaSchemaVersion,
		ConnectionErrors: &job.ConnectionErrors{}, // zero is an observation; nil would mean "never looked"
		Status:           job.StatusRunning,
		Provider:         r.opts.Provider,
		Model:            ptrIfNonEmpty(r.opts.Turn.Model),
		Effort:           ptrIfNonEmpty(r.opts.Turn.Effort),
		Cwd:              r.opts.Cwd,
		AllowWrite:       r.opts.Turn.AllowWrite,
		GitBaseline:      ptrIfNonEmpty(baseline),
		ResumedFrom:      ptrIfNonEmpty(r.opts.ResumedFrom),
		StartedAt:        job.ISO(r.startedAt),
		TimeoutMin:       r.opts.Turn.TimeoutMin,
		DeadlineAt:       deadlineAt,
		Label:            ptrIfNonEmpty(r.opts.Label),
		PromptFile:       promptFile,
		OutDir:           r.ws.Dir,
		RawPath:          r.ws.RawLogPath(),
		StderrPath:       r.ws.StderrLogPath(),
		ProgressPath:     r.ws.ProgressLogPath(),
		WatchCommand:     r.ws.WatchCommand(),
		ProviderArgv:     append([]string{r.opts.Provider}, r.argv...),
		RunnerPid:        os.Getpid(),
		RunnerInstanceID: r.instance,
		PromptState:      job.PromptUnknown,
		ResultKind:       job.ResultNone,
		NextAction:       prose.RunningNext(r.ws.Dir, r.ws.WatchCommand()),
	}
	r.setSession(r.driver.PreflightSessionID())
}

// writeMeta applies the caller's mutation to the in-memory meta — the single
// source of truth — and atomically replaces meta.json with the snapshot.
func (r *run) writeMeta(mutate func(*job.Meta)) {
	if mutate != nil {
		mutate(r.meta)
	}
	if err := r.meta.WriteFile(r.ws.MetaPath()); err != nil {
		fmt.Fprintf(r.opts.Stderr, "meta write warning: %s\n", err)
	}
}

func (r *run) printStartupBlock() {
	var block bytes.Buffer
	w := &block
	display := func(v string) string {
		if v == "" {
			return "(provider default)"
		}
		return v
	}
	fmt.Fprintf(w, "out-dir: %s\n", r.ws.Dir)
	fmt.Fprintf(w, "provider: %s · model %s · effort %s · hard cap %s\n",
		r.opts.Provider, display(r.opts.Turn.Model), display(r.opts.Turn.Effort), text.HardCap(r.opts.Turn.TimeoutMin))
	fmt.Fprintf(w, "watch: %s\n", r.ws.WatchCommand())
	fmt.Fprintf(w, "raw: %s\n", r.ws.RawLogPath())
	fmt.Fprintf(w, "stderr: %s\n", r.ws.StderrLogPath())
	if r.meta.GitBaseline != nil {
		fmt.Fprintf(w, "baseline: %s\n", *r.meta.GitBaseline)
	}
	if r.opts.ResumedFrom != "" {
		fmt.Fprintf(w, "resumed-from: %s\n", r.opts.ResumedFrom)
	}
	if r.session() != "" {
		fmt.Fprintf(w, "session: %s\n", r.session())
		fmt.Fprintf(w, "takeover-after-terminal: %s\n", r.driver.Takeover())
	}
	fmt.Fprintf(w, "next: %s\n", prose.DispatchNext(r.ws.Dir))
	r.opts.Stdout.Write(block.Bytes())
	if r.opts.CoordinateFile != "" {
		if err := job.WriteCoordinateFile(r.opts.CoordinateFile, block.Bytes()); err != nil {
			fmt.Fprintf(r.opts.Stderr, "coordinate file warning: %s\n", err)
		}
	}
}

type finishArgs struct {
	status              string
	text                string // final text, ok only
	errorText           string
	recovery            string // the prescription; prose owns its wording
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
	collectAction := prose.CollectThisJob(r.ws.Dir)

	var recoveryAction *string
	if f.status != job.StatusOK {
		recovery := f.recovery
		if recovery == "" {
			recovery = prose.Recovery(r.meta.PromptState, r.resumeCommand(), "")
		}
		recoveryAction = job.Ptr(recovery)
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
	r.releaseLock()

	w := r.opts.Stdout
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "status: %s\n", prose.StatusLine(f.status))
	fmt.Fprintf(w, "result: %s\n", r.ws.ResultPath())
	fmt.Fprintf(w, "meta: %s\n", r.ws.MetaPath())
	session := r.session()
	if session == "" {
		session = "(none)"
	}
	fmt.Fprintf(w, "session: %s\n", session)
	if r.session() != "" && !r.conflicted() {
		fmt.Fprintf(w, "takeover: %s\n", r.driver.Takeover())
	}
	fmt.Fprintf(w, "next: %s\n", collectAction)

	r.exitCode = job.ExitCodeFor(f.status)
}

func elapsed(since time.Time) string {
	return text.FormatDuration(time.Since(since))
}

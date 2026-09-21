package runner

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/gitx"
	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
	"github.com/qiushiyan/envoy/internal/provider"
	"github.com/qiushiyan/envoy/internal/text"
)

func (r *run) initMeta() {
	baseline := r.opts.Baseline
	if baseline == "" && r.opts.Turn.AllowWrite {
		// The review anchor: a write turn defaults to HEAD so collect can
		// always diff the delegate's work.
		baseline = gitx.Head(r.opts.Cwd)
	}
	var deadlineAt *string
	if !r.deadline.IsZero() {
		deadlineAt = job.Ptr(job.ISO(r.deadline))
	}
	// Compute native argv once; record its launch prefix alongside it before spawn.
	r.argv = r.driver.Argv()
	r.meta = &job.Meta{
		SchemaVersion:    job.MetaSchemaVersion,
		ConnectionErrors: connectionTally(r.driver),
		Usage:            r.driver.Usage(),
		Status:           job.StatusRunning,
		Provider:         r.opts.Provider,
		Model:            ptrIfNonEmpty(r.opts.Turn.Model),
		Effort:           ptrIfNonEmpty(r.opts.Turn.Effort),
		Cwd:              r.opts.Cwd,
		AllowWrite:       r.opts.Turn.AllowWrite,
		GitBaseline:      ptrIfNonEmpty(baseline),
		MaxBudgetUSD:     r.opts.Turn.MaxBudgetUSD,
		ResumedFrom:      ptrIfNonEmpty(r.opts.ResumedFrom),
		StartedAt:        job.ISO(r.startedAt),
		TimeoutMin:       r.opts.TimeoutMin,
		DeadlineAt:       deadlineAt,
		ProviderArgv:     append([]string{r.opts.Provider}, r.argv...),
		CommandPrefix:    commandPrefix(r.opts.Provider),
		RunnerPid:        os.Getpid(),
		RunnerInstanceID: r.instance,
		PromptState:      job.PromptUnknown,
		ResultKind:       job.ResultNone,
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

// printStartupBlock is the dispatch's own stdout: where the job is and what
// to do once the process exits. The caller named the job, so nothing here is
// a coordinate it has to read back.
func (r *run) printStartupBlock() {
	w := r.opts.Stdout
	display := func(v string) string {
		if v == "" {
			return "(provider default)"
		}
		return v
	}
	fmt.Fprintf(w, "job: %s\n", r.ws.Dir)
	fmt.Fprintf(w, "provider: %s · model %s · effort %s · hard cap %s\n",
		r.opts.Provider, display(r.opts.Turn.Model), display(r.opts.Turn.Effort), text.HardCap(r.opts.TimeoutMin))
	if line := prose.Launcher(r.meta); line != "" {
		fmt.Fprintln(w, line)
	}
	if r.meta.GitBaseline != nil {
		fmt.Fprintf(w, "baseline: %s\n", *r.meta.GitBaseline)
	}
	if r.opts.ResumedFrom != "" {
		fmt.Fprintf(w, "resumed-from: %s\n", r.opts.ResumedFrom)
	}
	fmt.Fprintf(w, "next: %s\n", prose.DispatchNext(r.ws.Dir))
}

type finishArgs struct {
	status              string
	text                string // final text, ok only
	errorText           string
	remedy              string // the driver's cause-specific fix, "" when none
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
		m.Usage = r.driver.Usage()
		m.Usage.End()
		m.CostUSD = f.costUSD
		if f.status == job.StatusOK {
			m.Error = nil
			m.Remedy = nil
		} else {
			m.Error = ptrIfNonEmpty(f.errorText)
			m.Remedy = ptrIfNonEmpty(f.remedy)
		}
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
	fmt.Fprintf(w, "next: %s\n", collectAction)

	r.exitCode = job.ExitCodeFor(f.status)
}

func elapsed(since time.Time) string {
	return text.FormatDuration(time.Since(since))
}

// connectionTally starts the connection-error tally only for a driver that
// recognizes such events: for it, zero is an observation; for any other, a
// zero would claim a link held that nothing watched, so the field stays nil.
func connectionTally(d provider.Driver) *job.ConnectionErrors {
	if d != nil && d.ObservesConnectionErrors() {
		return &job.ConnectionErrors{}
	}
	return nil
}

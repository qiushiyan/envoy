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
	if baseline == "" && r.opts.AllowWrite {
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
		Model:            job.PtrIfNonEmpty(r.opts.Turn.Model),
		Effort:           job.PtrIfNonEmpty(r.opts.Turn.Effort),
		Cwd:              r.opts.Cwd,
		AllowWrite:       r.opts.AllowWrite,
		GitBaseline:      job.PtrIfNonEmpty(baseline),
		MaxBudgetUSD:     r.opts.Turn.MaxBudgetUSD,
		ResumedFrom:      job.PtrIfNonEmpty(r.opts.ResumedFrom),
		Caller:           job.PtrIfNonEmpty(r.opts.Caller),
		StartedAt:        job.ISO(r.startedAt),
		TimeoutMin:       r.opts.TimeoutMin,
		DeadlineAt:       deadlineAt,
		ProviderArgv:     append([]string{r.opts.Provider}, r.argv...),
		CommandPrefix:    commandPrefix(r.opts.Provider),
		RunnerPid:        os.Getpid(),
		RunnerLock:       r.claimed,
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
	fmt.Fprintf(w, "job: %s\n", r.ws.Dir)
	fmt.Fprintf(w, "provider: %s · model %s · effort %s · hard cap %s\n",
		r.opts.Provider, prose.Setting(r.opts.Turn.Model), prose.Setting(r.opts.Turn.Effort), text.HardCap(r.opts.TimeoutMin))
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

// finish publishes how the turn ended — a driver's conclusion, or one the
// runner assembled for an ending the driver did not see through — as terminal
// state: result.md, final meta, the terminal coordinate block, and the exit
// code. It runs once: from onChildDone, or for a provider that never spawned.
func (r *run) finish(out provider.Outcome, exit exitResult) {
	if out.SessionID != "" {
		r.setSession(out.SessionID)
	}
	// An ending proved at the last moment goes through the same path a
	// streamed proof takes, so its time and progress line are recorded too;
	// a turn already accepted keeps its first evidence.
	if out.PromptState == job.PromptAccepted {
		r.markPromptAccepted(out.Evidence)
	}
	endedAt := time.Now()
	m := r.meta
	m.Status = out.Status
	m.EndedAt = job.Ptr(job.ISO(endedAt))
	m.DurationMs = job.Ptr(endedAt.Sub(r.startedAt).Milliseconds())
	m.Tokens = out.Tokens
	m.Usage = r.driver.Usage()
	m.Usage.End()
	m.CostUSD = out.CostUSD
	m.Failure = out.Failure
	if out.PromptState == job.PromptNotStarted {
		m.PromptState = out.PromptState
		m.PromptStateEvidence = job.PtrIfNonEmpty(out.Evidence)
	}
	m.ChildExitCode = exit.code
	m.ChildExitSignal = exit.signal
	m.CollectedAt = nil

	// A non-ok turn's result.md is worded from the record just completed, so
	// it says exactly what a later collect will.
	resultBody := out.Text
	m.ResultKind = job.ResultFinal
	if out.Status != job.StatusOK {
		partial := ""
		if out.Partial != nil && strings.TrimSpace(*out.Partial) != "" {
			partial = *out.Partial
		}
		m.ResultKind = job.ResultNone
		if partial != "" {
			m.ResultKind = job.ResultPartial
		}
		resultBody = prose.FailedResult(m, partial)
	}
	// The payload lands before the record that declares the turn terminal.
	if err := job.WriteFileAtomic(r.ws.ResultPath(), []byte(resultBody)); err != nil {
		fmt.Fprintf(r.opts.Stderr, "result write warning: %s\n", err)
	}
	r.writeMeta(nil)
	r.progress.Append("terminal",
		job.KV{K: "status", V: out.Status},
		job.KV{K: "elapsed", V: elapsed(r.startedAt)},
		job.KV{K: "result", V: m.ResultKind},
		job.KV{K: "prompt", V: m.PromptState},
	)
	r.releaseLock()

	fmt.Fprint(r.opts.Stdout, prose.TurnEnded(r.ws.Dir, out.Status, job.Deref(r.meta.SessionID)))

	r.exitCode = job.ExitCodeFor(out.Status)
}

func elapsed(since time.Time) string {
	return text.FormatDuration(time.Since(since))
}

// connectionTally starts the connection-error tally only for a driver that
// recognizes such events: for it, zero is an observation; for any other, a
// zero would claim a link held that nothing watched, so the field stays nil.
func connectionTally(d provider.Driver) *job.ConnectionErrors {
	if d.ObservesConnectionErrors() {
		return &job.ConnectionErrors{}
	}
	return nil
}

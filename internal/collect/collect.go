// Package collect recovers or prints one finished (or stranded) job as a
// single scannable block, and discovers pending jobs after a possibly missed
// completion notification. Explicit collection stamps terminal jobs with
// collectedAt once their block has reached the caller; pending discovery
// never marks anything collected.
//
// Every command and prescription a caller reads is rendered here, from the
// job's records and the directory they were read from. The runner records
// facts; this package words them.
package collect

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/qiushiyan/envoy/internal/gitx"
	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
)

// Mode selects which sections of a job's block collection prints. The default
// prints everything; the narrowed modes exist because the block is read by an
// agent whose context the result body and the status preamble compete for.
type Mode int

const (
	// ModeFull prints the whole block and stamps terminal collection.
	ModeFull Mode = iota
	// ModeResultOnly prints an ok turn's result body alone and stamps it
	// collected. A turn that is not ok prints its full block instead: its
	// status and next action are its result, and suppressing them would hand
	// the caller a payload that does not exist.
	ModeResultOnly
	// ModeStatusOnly prints everything except the result body and stamps
	// nothing: the result was not delivered, so the job stays uncollected.
	// It is also the full-preamble read: a healthy turn's own block holds
	// back the diagnostic fields, and this is the flag that asks for them
	// without a status having to go wrong first.
	ModeStatusOnly
)

// Collect prints one job — a single turn, or a whole fan-out with a section per
// member — and stamps first terminal collection once the block has reached
// the caller. Returns a process exit code.
//
// Rendering and acknowledgment are separate steps: the block is rendered in
// full first, written to the caller second, and only a write that succeeded
// stamps anything. A caller whose output broke mid-block is still owed the
// job, and pending discovery keeps listing it.
func Collect(dir, note string, mode Mode, w, errW io.Writer) int {
	if job.IsGroupDir(dir) {
		return collectGroup(dir, note, mode, w, errW)
	}
	meta, err := job.ReadMeta(dir)
	if err != nil {
		reportUnreadable(dir, err, errW)
		return job.ExitUsage
	}
	var body bytes.Buffer
	r := renderJob(dir, meta, mode, &body, true)
	if !deliver(w, errW, withNote(body.Bytes(), note, mode, errW)) {
		return job.ExitInfra
	}
	if r.stamp {
		stampCollected(dir, r.meta)
	}
	return 0
}

// withNote places what the caller must know about how its reference was
// resolved directly under the block's first line, which names the job it
// resolved to. A result-only read is the payload alone, so there the note
// goes to the error stream instead of into the result.
func withNote(body []byte, note string, mode Mode, errW io.Writer) []byte {
	if note == "" {
		return body
	}
	if mode == ModeResultOnly {
		fmt.Fprintf(errW, "note: %s\n", note)
		return body
	}
	head, rest, _ := bytes.Cut(body, []byte("\n"))
	return slices.Concat(head, []byte("\nnote: "+note+"\n"), rest)
}

// deliver writes a rendered block to the caller and reports whether all of
// it arrived. A short or failed write is reported on stderr so the caller
// knows the job is still owed.
func deliver(w, errW io.Writer, body []byte) bool {
	n, err := w.Write(body)
	if err == nil && n < len(body) {
		err = io.ErrShortWrite
	}
	if err != nil {
		fmt.Fprintf(errW, "collect error: the block was not delivered (%s), so nothing was marked collected; collect again once the output is readable\n", err)
		return false
	}
	return true
}

// reportUnreadable says on the error stream why a turn's record could not be
// read, and where the evidence of what it did still is.
func reportUnreadable(dir string, err error, errW io.Writer) {
	ws := job.Workspace{Dir: dir}
	if errors.Is(err, job.ErrNoRecord) {
		fmt.Fprintf(errW,
			"collect error: %s not found — inspect %s, %s, %s, and the working tree before deciding whether to retry\n",
			ws.MetaPath(), ws.ProgressLogPath(), ws.RawLogPath(), ws.StderrLogPath())
		return
	}
	fmt.Fprintf(errW,
		"collect error: %s could not be read (%s). Inspect %s, %s, %s, and the working tree; do not assume the job finished or retry it blindly.\n",
		ws.MetaPath(), err, ws.ProgressLogPath(), ws.RawLogPath(), ws.StderrLogPath())
}

// rendered is what one turn's block decided while it was being written: the
// record it described, whether an ok payload failed to reach the block, and
// whether delivering the block completes the collection.
type rendered struct {
	meta        *job.Meta
	undelivered bool // ok status, but result.md could not be read
	stamp       bool // deliverable complete: stamp once the block is written
}

// renderJob writes one turn's block into w and reports what a fan-out needs
// to aggregate its members — status and delivery both — without a second
// reader of meta.json. showGit is false for a member of a fan-out, where the
// reviewed range belongs to the whole fan-out and is printed once above the
// members rather than per member.
func renderJob(dir string, meta *job.Meta, mode Mode, w io.Writer, showGit bool) rendered {
	state := classifyRunning(dir, meta)
	meta = reconcileAbandoned(dir, meta, state)
	state = classifyRunning(dir, meta)

	// The payload is read once, before anything is printed, because whether it
	// reads is what the rest of the block is shaped around — not a detail
	// discovered on the way past.
	resultBody, resultErr := os.ReadFile(job.Workspace{Dir: dir}.ResultPath())

	// The result-only shortcut applies only when the payload actually reads:
	// an ok turn whose result.md is unreadable falls through to the full
	// block, which diagnoses the missing payload and leaves the job owed.
	if mode == ModeResultOnly && meta.Status == job.StatusOK && resultErr == nil {
		fmt.Fprintln(w, string(resultBody))
		return rendered{meta: meta, stamp: true}
	}

	// What the block holds back is decided by delivery, not by status. A turn
	// delivers when it reports ok *and* its payload actually reads; only then
	// is the caller done, and only then is the rest of the preamble — the
	// settings it passed itself, the token counts, the prompt-state evidence,
	// the result kind, the log paths — answering a question nobody asked.
	// Status alone would get the two cases at the edges wrong: an ok turn
	// whose result.md will not read is sent to raw.log by its own next line
	// and must be given the paths, and --status-only delivers nothing at all.
	// Nothing is lost either way: meta.json stays the authoritative record.
	delivered := meta.Status == job.StatusOK && resultErr == nil
	writePreamble(w, dir, meta, state, !delivered || mode == ModeStatusOnly, showGit)
	if meta.GitBaseline != nil && showGit {
		printGitSinceBaseline(w, meta.Cwd, *meta.GitBaseline)
	}
	// The same read the tier decision was made from, so the block cannot
	// describe one payload and print another.
	if mode != ModeStatusOnly {
		fmt.Fprintln(w, "\n--- result.md ---")
		if resultErr == nil {
			fmt.Fprintln(w, string(resultBody))
		} else {
			fmt.Fprintln(w, "(no result.md yet)")
		}
	}
	next, r := closing(dir, meta, state, mode, resultErr == nil)
	fmt.Fprintf(w, "\nnext: %s\n", next)
	return r
}

// writePreamble writes everything above the payload: the coordinate and
// status, then — when the block carries its diagnostic tier — the settings,
// counts, evidence and log paths, and always the follow-up commands.
//
// standalone is false for a member of a fan-out, whose process is the whole
// fan-out's: stopping it is offered once, for the set, above the members.
func writePreamble(w io.Writer, dir string, meta *job.Meta, state prose.RunObservation, diagnostic, standalone bool) {
	ws := job.Workspace{Dir: dir}
	fmt.Fprintf(w, "job: %s\n", dir)
	if meta.Status == job.StatusRunning {
		fmt.Fprintf(w, "status: %s\n", prose.RunningStatus(state))
	} else {
		fmt.Fprintf(w, "status: %s\n", prose.StatusLine(meta.Status))
	}
	// The model column shows the request; when the provider announced what it
	// actually resolved, show that observation too.
	//
	// The two are deliberately NOT compared to decide whether to print. A
	// requested `opus` against a reported `claude-opus-5` is the provider's own
	// alias resolution — the mapping envoy refuses to own precisely because it
	// is the provider's and moves under the engine's feet — so string
	// inequality cannot tell an honest resolution from a substitution. Calling
	// one a surprise would be inferring, not observing, and would fire on the
	// most ordinary claude dispatch there is. The whole line therefore belongs
	// to the diagnostic tier, and the observation stays where it is provable:
	// meta.json's providerReportedModel, and --status-only.
	if diagnostic {
		fmt.Fprintf(w, "provider: %s · model %s · effort %s\n", meta.Provider,
			prose.ModelSetting(job.Deref(meta.Model), job.Deref(meta.ProviderReportedModel)),
			prose.Setting(job.Deref(meta.Effort)))
		if line := prose.Launcher(meta); line != "" {
			fmt.Fprintln(w, line)
		}
	}
	if meta.DurationMs != nil {
		costSuffix := ""
		if meta.CostUSD != nil {
			costSuffix = fmt.Sprintf(" · cost $%g", *meta.CostUSD)
		}
		fmt.Fprintf(w, "duration: %dm%s\n", int64(float64(*meta.DurationMs)/60000+0.5), costSuffix)
	}
	if diagnostic {
		fmt.Fprintf(w, "tokens: %s\n", prose.Tokens(meta.Tokens))
		if line := prose.ContextUsage(meta.Usage); line != "" {
			fmt.Fprintln(w, line)
		}
		prompt := meta.PromptState
		if prompt == "" {
			prompt = job.PromptUnknown
		}
		if meta.PromptStateEvidence != nil {
			prompt += " · evidence " + *meta.PromptStateEvidence
		}
		fmt.Fprintf(w, "prompt: %s\n", prompt)
		if meta.ResultKind != "" {
			fmt.Fprintf(w, "result kind: %s\n", meta.ResultKind)
		}
		if line := prose.ProviderStream(meta.ConnectionErrors); line != "" {
			fmt.Fprintln(w, line)
		}
		// The three streams are one affordance — "here is what the turn
		// actually emitted" — and a turn that did not deliver is the case that
		// wants it. Which of the three a given recovery line names varies (an
		// unprovable turn names all three, an accepted one names progress.log,
		// a never-started one names none), but that is emphasis, not
		// eligibility: gating each path on the branch that happened to fire
		// would make the block's shape unpredictable to save one line on a job
		// already in trouble.
		fmt.Fprintf(w, "logs: progress %s · raw %s · stderr %s\n", ws.ProgressLogPath(), ws.RawLogPath(), ws.StderrLogPath())
	}
	if meta.ResumedFrom != nil {
		fmt.Fprintf(w, "resumed-from: %s\n", *meta.ResumedFrom)
		// Both records say who dispatched them, so a turn that continued
		// another session's conversation — a name that fell back at dispatch
		// is how that happens unasked — says so where its result is read.
		if source, err := job.CallerOf(*meta.ResumedFrom); err == nil && meta.Caller != nil && source != "" && source != *meta.Caller {
			fmt.Fprintf(w, "note: %s\n", prose.ContinuedAnotherSession())
		}
	}
	if meta.SessionID != nil {
		// The bare id is printed only when no command carries it: the
		// continuation names the job, not the session, so a caller never
		// hand-assembles a follow-up out of the id. A running turn's session
		// still prints; its next line says to wait.
		if resume := resumeCommand(dir, meta); resume == "" {
			fmt.Fprintf(w, "session: %s\n", *meta.SessionID)
		} else {
			fmt.Fprintf(w, "resume: %s\n", resume)
		}
	}
	// A turn outlives the command that dispatched it, so stopping that
	// command stops only the waiting; the turn itself is stopped here.
	if standalone && state.State == prose.RunLive {
		fmt.Fprintf(w, "stop: %s\n", prose.StopCommand(state.RunnerPid))
	}
	if meta.Failure != nil {
		fmt.Fprintf(w, "error: %s\n", prose.Failure(meta))
	}
	if meta.CollectedAt != nil {
		fmt.Fprintf(w, "collected: %s\n", *meta.CollectedAt)
	}
}

// closing decides how one turn's block ends — its next line, and what
// delivering the block means — from the record, the mode, and whether the
// payload read. A still-running turn and a status check deliver nothing. An
// ok turn's deliverable is its payload; a non-ok turn's is the status and
// recovery above it. Only a delivered deliverable stamps collection, so an ok
// turn whose result.md cannot be read stays owed.
func closing(dir string, meta *job.Meta, state prose.RunObservation, mode Mode, payloadRead bool) (string, rendered) {
	r := rendered{meta: meta}
	switch {
	case state.State == prose.RunLive:
		return prose.RunningNext(dir), r
	case meta.Status == job.StatusRunning:
		return recoveryForStale(dir, meta, state), r
	case mode == ModeStatusOnly:
		return prose.StatusOnlyNext(dir), r
	case meta.Status == job.StatusOK && !payloadRead:
		r.undelivered = true
		return prose.OkResultUnreadable(dir), r
	case meta.Status == job.StatusOK:
		r.stamp = true
		return prose.CollectedOK(), r
	}
	r.stamp = true
	return recoveryAction(dir, meta), r
}

// stampCollected marks first terminal collection: the block has actually
// been delivered to a caller, so pending discovery stops listing the job.
func stampCollected(dir string, meta *job.Meta) {
	if meta.CollectedAt != nil {
		return
	}
	meta.CollectedAt = job.Ptr(job.ISO(time.Now()))
	meta.WriteFile(job.Workspace{Dir: dir}.MetaPath())
}

// printGitSinceBaseline shows what changed in the tree since a turn's anchor:
// the commits, the diffstat, and whether anything is still uncommitted.
func printGitSinceBaseline(w io.Writer, cwd, baseline string) {
	fmt.Fprintf(w, "\n--- git since baseline %s (in %s) ---\n", baseline, cwd)
	fmt.Fprintln(w, orDefault(gitx.Run(cwd, "log", baseline+"..HEAD", "--oneline"), "(no commits)"))
	fmt.Fprintln(w, orDefault(gitx.Run(cwd, "diff", baseline, "--stat"), "(no diff)"))
	if dirty := gitx.Run(cwd, "status", "--short"); dirty != "" {
		fmt.Fprintf(w, "dirty:\n%s\n", dirty)
	} else {
		fmt.Fprintln(w, "tree clean")
	}
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

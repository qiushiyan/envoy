package collect

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// turn is one turn as collect reads it. Every observation its block depends
// on is made here, before a line is written — what its processes show, the
// record reconciled when they show it abandoned, the one continuation read,
// the payload, and the records it names — so the block describes one reading,
// and a fan-out's header is derived from its members before any section is
// written. What the block decides (its next action, whether delivering it
// completes the collection) is read off these fields; writing it reads
// nothing but git, whose output decides nothing.
type turn struct {
	dir   string
	meta  *job.Meta
	state prose.RunObservation
	cont  continuation
	// result is result.md as read once, and resultErr why it would not read.
	result    []byte
	resultErr error
	// continuedAnother is whether the conversation this turn continued was
	// begun by a different recorded caller than the one that dispatched it.
	continuedAnother bool
	// fanOut is the fan-out a live turn runs in, "" for a job of its own.
	fanOut string
}

// readTurn reads the turn in dir from its record. A turn whose processes are
// provably gone while its record says running is reconciled first, so the
// rest of the reading — and the block — describe the reconciled record; the
// processes are probed once, and the reconciliation and the block both act
// on that one observation.
func readTurn(dir string, meta *job.Meta) turn {
	t := turn{dir: dir, meta: meta, state: classifyRunning(dir, meta)}
	if t.state.State == prose.RunAbandoned {
		t.meta = reconcileAbandoned(dir, meta)
		t.state = prose.RunObservation{}
	}
	// Like the payload, the continuation rule is read once: the archive line,
	// the resume line and the recovery all describe that one read.
	t.cont = continuationOf(dir, t.meta)
	// The payload is read once, before anything is printed, because whether it
	// reads is what the rest of the block is shaped around — not a detail
	// discovered on the way past.
	t.result, t.resultErr = os.ReadFile(job.Workspace{Dir: dir}.ResultPath())
	// Both records say who dispatched them, so a turn that continued another
	// session's conversation — a name that fell back at dispatch is how that
	// happens unasked — says so where its result is read.
	if from := t.meta.ResumedFrom; from != nil && t.meta.Caller != nil {
		source, err := job.CallerOf(*from)
		t.continuedAnother = err == nil && source != "" && source != *t.meta.Caller
	}
	if t.state.State == prose.RunLive {
		if group := filepath.Dir(dir); job.IsGroupDir(group) {
			t.fanOut = group
		}
	}
	return t
}

// delivered is whether the turn's payload landed: it reports ok and its
// result.md reads. Only then is the caller done with it.
func (t turn) delivered() bool { return t.meta.Status == job.StatusOK && t.resultErr == nil }

// payloadLost is a turn that reports ok whose result.md will not read: its
// answer exists nowhere the block can print it.
func (t turn) payloadLost() bool { return t.meta.Status == job.StatusOK && t.resultErr != nil }

// resumable is whether the turn's conversation may continue, by the one rule.
func (t turn) resumable() bool { return t.cont.blocker == "" }

// stamps is whether delivering this turn's block delivers its deliverable: an
// ok turn's payload, or a finished turn's status and recovery when it did not
// deliver. A still-running turn and a status check deliver nothing, and an ok
// turn whose result.md cannot be read stays owed.
func (t turn) stamps(mode Mode) bool {
	return mode != ModeStatusOnly && t.meta.Status != job.StatusRunning && !t.payloadLost()
}

// undelivered is whether this turn's block, written in mode, leaves an ok
// result undelivered — what a fan-out's closing must not fold into "every
// result above is usable".
func (t turn) undelivered(mode Mode) bool { return mode != ModeStatusOnly && t.payloadLost() }

// next is how the turn's block ends: the one action its reading licenses.
func (t turn) next(mode Mode) string {
	switch {
	case t.state.State == prose.RunLive:
		return prose.RunningNext(t.dir)
	case t.meta.Status == job.StatusRunning:
		return recoveryForStale(t.dir, t.meta, t.state, t.cont)
	case mode == ModeStatusOnly:
		return prose.StatusOnlyNext(t.dir)
	case t.payloadLost():
		return prose.OkResultUnreadable(t.dir)
	case t.meta.Status == job.StatusOK:
		return prose.CollectedOK()
	}
	return recoveryAction(t.dir, t.meta, t.cont)
}

// write writes the turn's block. section is true when the turn is written as
// a section of its fan-out's block, where what belongs to the whole fan-out —
// the reviewed range, the stop — is printed once above the members instead.
func (t turn) write(w io.Writer, mode Mode, section bool) {
	// The result-only shortcut applies only when the payload actually reads:
	// an ok turn whose result.md is unreadable falls through to the full
	// block, which diagnoses the missing payload and leaves the job owed.
	if mode == ModeResultOnly && t.delivered() {
		fmt.Fprintln(w, string(t.result))
		return
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
	t.writePreamble(w, !t.delivered() || mode == ModeStatusOnly, section)
	if t.meta.GitBaseline != nil && !section {
		printGitSinceBaseline(w, t.meta.Cwd, *t.meta.GitBaseline)
	}
	// The same read the tier decision was made from, so the block cannot
	// describe one payload and print another.
	if mode != ModeStatusOnly {
		fmt.Fprintln(w, "\n--- result.md ---")
		if t.resultErr == nil {
			fmt.Fprintln(w, string(t.result))
		} else {
			fmt.Fprintln(w, "(no result.md yet)")
		}
	}
	fmt.Fprintf(w, "\nnext: %s\n", t.next(mode))
}

// writePreamble writes everything above the payload: the coordinate and
// status, then — when the block carries its diagnostic tier — the settings,
// counts, evidence and log paths, and always the follow-up commands.
func (t turn) writePreamble(w io.Writer, diagnostic, section bool) {
	meta, ws := t.meta, job.Workspace{Dir: t.dir}
	fmt.Fprintf(w, "job: %s\n", t.dir)
	if meta.Status == job.StatusRunning {
		fmt.Fprintf(w, "status: %s\n", prose.RunningStatus(t.state))
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
	// What the session was set up with is a setting like the model, held back
	// with it — unless the archive no longer supplies it, which is why the
	// block offers no resume: and so is printed whatever the tier.
	if meta.InstructionsSha256 != nil && (diagnostic || t.cont.problem != "") {
		fmt.Fprintf(w, "instructions: %s\n", prose.InstructionsArchive(t.dir, t.cont.problem))
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
		if t.continuedAnother {
			fmt.Fprintf(w, "note: %s\n", prose.ContinuedAnotherSession())
		}
	}
	if meta.SessionID != nil {
		// The bare id is printed only when no command carries it: the
		// continuation names the job, not the session, so a caller never
		// hand-assembles a follow-up out of the id. A running turn's session
		// still prints; its next line says to wait.
		if resume := resumeCommand(t.dir, meta, t.cont); resume == "" {
			fmt.Fprintf(w, "session: %s\n", *meta.SessionID)
		} else {
			fmt.Fprintf(w, "resume: %s\n", resume)
		}
	}
	// A turn outlives the command that dispatched it, so stopping that
	// command stops only the waiting; the turn itself is stopped here.
	if !section && t.state.State == prose.RunLive {
		fmt.Fprintf(w, "stop: %s\n", prose.StopCommand(t.state.RunnerPid))
		// A member read on its own runs in its fan-out's one process, so its
		// stop is the fan-out's, and stops every member with it.
		if t.fanOut != "" {
			fmt.Fprintf(w, "note: %s\n", prose.StopReachesEveryMember(t.fanOut))
		}
	}
	if meta.Failure != nil {
		fmt.Fprintf(w, "error: %s\n", prose.Failure(meta))
	}
	if meta.CollectedAt != nil {
		fmt.Fprintf(w, "collected: %s\n", *meta.CollectedAt)
	}
}

// classifyRunning reads a turn recorded as running by what its processes
// show; a turn that is not running observes nothing.
func classifyRunning(dir string, meta *job.Meta) prose.RunObservation {
	if meta.Status != job.StatusRunning {
		return prose.RunObservation{}
	}
	runnerAlive := proc.RunnerLiveness(dir, meta.RunnerPid, meta.RunnerLock)
	pgid := 0
	if meta.ProviderPgid != nil {
		pgid = *meta.ProviderPgid
	}
	pid := 0
	if meta.ProviderPid != nil {
		pid = *meta.ProviderPid
	}
	groupAlive := proc.GroupLiveness(pgid)
	providerAlive := proc.PidLiveness(pid)

	switch {
	case runnerAlive == proc.Live:
		return prose.RunObservation{State: prose.RunLive, RunnerPid: meta.RunnerPid}
	case groupAlive == proc.Live || providerAlive == proc.Live:
		id := pgid
		if id == 0 {
			id = pid
		}
		return prose.RunObservation{State: prose.RunOrphaned, ProviderGroup: id}
	case runnerAlive == proc.Gone && (groupAlive == proc.Gone || (meta.ProviderPgid == nil && providerAlive == proc.Gone)):
		return prose.RunObservation{State: prose.RunAbandoned}
	}
	return prose.RunObservation{State: prose.RunUnknown}
}

// reconcileAbandoned rewrites a provably abandoned running job as terminal
// status "abandoned" so its evidence survives.
func reconcileAbandoned(dir string, meta *job.Meta) *job.Meta {
	ws := job.Workspace{Dir: dir}
	meta.Status = job.StatusAbandoned
	meta.ReconciledAt = job.Ptr(job.ISO(time.Now()))
	meta.Usage.End()
	meta.Failure = &job.Failure{Cause: job.CauseAbandoned}
	if _, err := os.Stat(ws.ResultPath()); os.IsNotExist(err) {
		job.WriteFileAtomic(ws.ResultPath(), []byte(prose.FailedResult(meta, "")))
	}
	meta.WriteFile(ws.MetaPath())
	return meta
}

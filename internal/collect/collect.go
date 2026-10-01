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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/gitx"
	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// jobDirs lists job directories under base, sorted by name. A dir counts as a
// job once it holds a record — meta.json for a turn, group.json for a fan-out —
// since a record is what a reader can act on; a directory reserved and
// released without one was never a job. It returns the read error rather than
// swallowing it: a store that could not be read is not an empty store.
func jobDirs(base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if dir := filepath.Join(base, e.Name()); job.HasRecord(dir) {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// ---------- continuing a finished job ----------

// continuationBlocker is the single definition of "this job's session may
// not be continued", consulted by every surface that advertises or dispatches
// a continuation: collect's resume line, the fan-out's set-level resume line,
// and the @<job> inspection at dispatch. One definition means no surface can
// advertise a follow-up that dispatch would refuse. A recorded session-lock
// conflict blocks because the session may belong to another job.
func continuationBlocker(meta *job.Meta) (prose.ResumeBlockerKind, bool) {
	switch {
	case meta == nil:
		return prose.BlockerUnreadableMeta, true
	case meta.Status == job.StatusRunning:
		return prose.BlockerRunning, true
	case meta.SessionLockConflict != nil:
		return prose.BlockerLockConflict, true
	case meta.SessionID == nil:
		return prose.BlockerNoSession, true
	}
	return "", false
}

// Inspect reads dir as a single turn and reports whether its conversation
// can be continued; see Continuable.
func Inspect(dir string) (*job.Meta, prose.ResumeBlockerKind, error) {
	return Continuable(job.ReadMeta(dir))
}

// Continuable judges a turn record as read — the record and its read error —
// returning the record a continuation inherits its settings from, whose
// session is then always set. The error is non-nil only when there is no
// readable turn at all; a real turn that cannot continue reports a blocker
// instead.
func Continuable(meta *job.Meta, err error) (*job.Meta, prose.ResumeBlockerKind, error) {
	if errors.Is(err, job.ErrNoRecord) {
		return nil, "", errors.New("no job found there (no meta.json)")
	}
	if err != nil {
		return nil, "", err
	}
	if !job.KnownStatus(meta.Status) || meta.Provider == "" {
		return nil, "", fmt.Errorf("meta.json is not a job this engine wrote (status %q)", meta.Status)
	}
	if kind, blocked := continuationBlocker(meta); blocked {
		return nil, kind, nil
	}
	return meta, "", nil
}

// NameHold is the single definition of "this job still holds its name": it
// is undelivered — some turn in it not yet terminal and collected, which is
// what collectedAt means — so a caller may still be waiting to collect it by
// that name. Whom the hold binds is the dispatcher's question (a job holds
// its name only against a caller that shares its meaning of the name); this
// says only whether there is anything left to deliver. Age is not consulted:
// a job nobody collected holds until someone does, and pending lists it. A
// directory with no record holds too: it is a dispatch between its
// reservation and its first record, or one that died there, and the engine
// cannot tell which.
//
// A fan-out holds through any member that does. A member the roster names
// that has no record — a session held at dispatch refuses that member alone,
// before it writes anything — has nothing a collect could ever deliver, but
// its absence alone does not say it was refused: members start independently,
// and one may still be preparing its turn. What says it never will start is
// the supervising process being gone, so a recordless member holds for as
// long as that process may be alive, and an older manifest that does not name
// it holds for good.
func NameHold(dir string) (prose.NameHold, bool) {
	if !job.IsGroupDir(dir) {
		return turnHold(dir)
	}
	fan, err := job.ReadFan(dir)
	if err != nil {
		return prose.NameHold{Kind: prose.HoldUnreadable}, true
	}
	mayStillStart := proc.PidLiveness(fan.Group.RunnerPid) != proc.Gone
	for _, m := range fan.Members {
		if errors.Is(m.Err, job.ErrNoRecord) {
			if mayStillStart {
				return prose.NameHold{Kind: prose.HoldUnrecorded, Member: m.Name}, true
			}
			continue
		}
		if hold, held := recordHold(m.Meta, m.Err); held {
			hold.Member = m.Name
			return hold, true
		}
	}
	return prose.NameHold{}, false
}

func turnHold(dir string) (prose.NameHold, bool) {
	meta, err := job.ReadMeta(dir)
	if errors.Is(err, job.ErrNoRecord) {
		entries, err := os.ReadDir(dir)
		return prose.NameHold{Kind: prose.HoldUnrecorded, Empty: err == nil && len(entries) == 0}, true
	}
	return recordHold(meta, err)
}

// recordHold is the hold a turn record — as read — places on its name.
func recordHold(meta *job.Meta, err error) (prose.NameHold, bool) {
	switch {
	case err != nil:
		return prose.NameHold{Kind: prose.HoldUnreadable}, true
	case meta.Status == job.StatusRunning:
		return prose.NameHold{Kind: prose.HoldRunning}, true
	case meta.CollectedAt == nil:
		return prose.NameHold{Kind: prose.HoldUncollected}, true
	}
	return prose.NameHold{}, false
}

// ---------- classification and recovery ----------

// runningState classifies a status:"running" meta by process liveness.
type runningState struct {
	kind   string // "terminal" | "live" | "orphaned" | "abandoned" | "unknown"
	detail string
}

func classifyRunning(meta *job.Meta) runningState {
	if meta.Status != job.StatusRunning {
		return runningState{kind: "terminal"}
	}
	runnerAlive := proc.PidLiveness(meta.RunnerPid)
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

	if runnerAlive == proc.Live {
		return runningState{
			kind:   "live",
			detail: fmt.Sprintf("a process with recorded runner PID %d is alive", meta.RunnerPid),
		}
	}
	if groupAlive == proc.Live || providerAlive == proc.Live {
		id := pgid
		if id == 0 {
			id = pid
		}
		return runningState{
			kind:   "orphaned",
			detail: fmt.Sprintf("the runner is gone, but a process in the recorded provider group %d is still alive", id),
		}
	}
	if runnerAlive == proc.Gone && (groupAlive == proc.Gone || (meta.ProviderPgid == nil && providerAlive == proc.Gone)) {
		return runningState{
			kind:   "abandoned",
			detail: "the recorded runner and provider process group are no longer alive",
		}
	}
	return runningState{
		kind:   "unknown",
		detail: "the job's process records are incomplete, so provider liveness cannot be established safely",
	}
}

// resumeCommand is the follow-up that continues this job's conversation, or
// "" when continuing it is not licensed — the same eligibility an @<job>
// voice applies, so collect can never offer a continuation dispatch would
// refuse.
func resumeCommand(dir string, meta *job.Meta) string {
	if _, blocked := continuationBlocker(meta); blocked {
		return ""
	}
	return prose.ContinueCommand(dir, meta.TimeoutMin)
}

// recoveryAction is the prescription for a terminal job that did not
// deliver, rendered from its records: what the prompt state licenses, the
// driver's own remedy where it recorded one, and the commands that continue
// or repeat the dispatch.
func recoveryAction(dir string, meta *job.Meta) string {
	if meta.SessionLockConflict != nil {
		return prose.LockedSession(*meta.SessionLockConflict, false)
	}
	return prose.Recovery(meta, resumeCommand(dir, meta), prose.RedispatchCommand(dir, meta))
}

// recoveryForStale prescribes the next move for a job whose runner is gone.
// A live orphan provider outranks the prompt state: acting alongside it would
// put two turns in one tree.
func recoveryForStale(dir string, meta *job.Meta, state runningState) string {
	if meta.SessionLockConflict != nil {
		return prose.LockedSession(*meta.SessionLockConflict, state.kind == "orphaned")
	}
	switch state.kind {
	case "orphaned":
		return prose.Orphaned()
	case "abandoned":
		return recoveryAction(dir, meta)
	default:
		return prose.Unprovable()
	}
}

// reconcileAbandoned rewrites a provably abandoned running job as terminal
// status "abandoned" so its evidence survives.
func reconcileAbandoned(dir string, meta *job.Meta, state runningState) *job.Meta {
	if state.kind != "abandoned" {
		return meta
	}
	ws := job.Workspace{Dir: dir}
	errText := fmt.Sprintf("This turn ended without publishing a result: %s.", state.detail)
	meta.Status = job.StatusAbandoned
	meta.ReconciledAt = job.Ptr(job.ISO(time.Now()))
	meta.Usage.End()
	meta.Error = job.Ptr(errText)
	if _, err := os.Stat(ws.ResultPath()); os.IsNotExist(err) {
		job.WriteFileAtomic(ws.ResultPath(), []byte(fmt.Sprintf("# Turn abandoned\n\n%s\n", errText)))
	}
	meta.WriteFile(ws.MetaPath())
	return meta
}

func groupThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// ---------- collection ----------

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
	ws := job.Workspace{Dir: dir}
	state := classifyRunning(meta)
	meta = reconcileAbandoned(dir, meta, state)
	state = classifyRunning(meta)

	// The payload is read once, before anything is printed, because whether it
	// reads is what the rest of the block is shaped around — not a detail
	// discovered on the way past.
	resultBody, resultErr := os.ReadFile(ws.ResultPath())

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
	diagnostic := !delivered || mode == ModeStatusOnly

	fmt.Fprintf(w, "job: %s\n", dir)
	if meta.Status == job.StatusRunning {
		fmt.Fprintf(w, "status: running (%s — %s; result.md is not final)\n", state.kind, state.detail)
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
		tokens := "n/a"
		if pairs := meta.Tokens.Pairs(); len(pairs) > 0 {
			var parts []string
			for _, p := range pairs {
				parts = append(parts, fmt.Sprintf("%s %s", p[0], groupThousands(p[1].(int64))))
			}
			tokens = strings.Join(parts, " · ")
		}
		fmt.Fprintf(w, "tokens: %s\n", tokens)
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
	if meta.Error != nil {
		fmt.Fprintf(w, "error: %s\n", *meta.Error)
	}
	if meta.CollectedAt != nil {
		fmt.Fprintf(w, "collected: %s\n", *meta.CollectedAt)
	}

	if meta.GitBaseline != nil && showGit {
		printGitSinceBaseline(w, meta.Cwd, *meta.GitBaseline)
	}

	// The same read the tier decision was made from, so the block cannot
	// describe one payload and print another.
	resultDelivered := false
	if mode != ModeStatusOnly {
		fmt.Fprintln(w, "\n--- result.md ---")
		if resultErr == nil {
			fmt.Fprintln(w, string(resultBody))
			resultDelivered = true
		} else {
			fmt.Fprintln(w, "(no result.md yet)")
		}
	}

	if meta.Status == job.StatusRunning {
		if state.kind == "live" {
			fmt.Fprintf(w, "\nnext: %s\n", prose.RunningNext(dir))
		} else {
			fmt.Fprintf(w, "\nnext: %s\n", recoveryForStale(dir, meta, state))
		}
		return rendered{meta: meta}
	}

	if mode == ModeStatusOnly {
		fmt.Fprintf(w, "\nnext: %s\n", prose.StatusOnlyNext(dir))
		return rendered{meta: meta}
	}

	// An ok turn's deliverable is its payload; a non-ok turn's is the status
	// and recovery above. Only a delivered deliverable stamps collection, so
	// an ok turn whose result.md cannot be read stays owed.
	if meta.Status == job.StatusOK && !resultDelivered {
		fmt.Fprintf(w, "\nnext: %s\n", prose.OkResultUnreadable(dir))
		return rendered{meta: meta, undelivered: true}
	}

	if meta.Status == job.StatusOK {
		fmt.Fprintf(w, "\nnext: %s\n", prose.CollectedOK())
	} else {
		fmt.Fprintf(w, "\nnext: %s\n", recoveryAction(dir, meta))
	}
	return rendered{meta: meta, stamp: true}
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

// collectGroup prints a whole fan-out: the aggregate first, then one section
// per member, split by member name. The member sections are rendered before
// the header because collecting a member can change its status — reconciling
// an abandoned turn — and an aggregate that disagreed with the sections below
// it would be worse than no aggregate at all. Nothing is stamped until the
// whole block has reached the caller.
func collectGroup(dir, note string, mode Mode, w, errW io.Writer) int {
	fan, err := job.ReadFan(dir)
	if err != nil {
		fmt.Fprintf(errW,
			"collect error: %s could not be read (%s). Each member's job dir under %s is self-contained — collect one directly to see its result.\n",
			job.GroupWorkspace{Dir: dir}.GroupPath(), err, dir)
		return job.ExitUsage
	}
	group := fan.Group

	var sections bytes.Buffer
	statuses := make([]string, 0, len(fan.Members))
	labels := make([]string, 0, len(fan.Members))
	var undelivered []string
	type stampable struct {
		dir  string
		meta *job.Meta
	}
	var stamps []stampable
	resumable := len(fan.Members) > 0
	for _, m := range fan.Members {
		fmt.Fprintf(&sections, "\n=== member %s ===\n", m.Name)
		// A member with no record is the roster's observation, not the
		// member's: it gets its own section here, worded to what the directory
		// shows, rather than the single-turn error a turn with no record earns.
		if errors.Is(m.Err, job.ErrNoRecord) {
			fmt.Fprintf(&sections, "status: %s\nnext: %s\n", prose.FanMemberNoRecord(), prose.FanMemberNoRecordNext(m.Dir))
			statuses = append(statuses, "")
			labels = append(labels, m.Name+" no status")
			resumable = false
			continue
		}
		status := ""
		if m.Err != nil {
			reportUnreadable(m.Dir, m.Err, errW)
			fmt.Fprintf(&sections, "status: %s\n", prose.FanUndispatched())
			resumable = false
		} else {
			r := renderJob(m.Dir, m.Meta, mode, &sections, false)
			status = r.meta.Status
			if r.undelivered {
				undelivered = append(undelivered, m.Name)
			}
			if r.stamp {
				stamps = append(stamps, stampable{m.Dir, r.meta})
			}
			if _, blocked := continuationBlocker(r.meta); blocked {
				resumable = false
			}
		}
		statuses = append(statuses, status)
		label := status
		if label == "" {
			label = "no status"
		}
		labels = append(labels, m.Name+" "+label)
	}

	// The group's closing aggregates delivery alongside status: a member that
	// reports ok while its payload never printed must not be folded into
	// "every result above is usable".
	closing := func() string {
		if len(undelivered) > 0 {
			return prose.FanUndeliveredResults(undelivered)
		}
		return prose.FanCollected(dir, statuses)
	}

	var body bytes.Buffer
	if mode == ModeResultOnly {
		// Result-only keeps the aggregate line and the member split —
		// attribution is the point of a fan-out — and drops the preamble.
		fmt.Fprintf(&body, "status: %s\n", prose.FanStatusLine(statuses))
		body.Write(sections.Bytes())
		if len(undelivered) > 0 || prose.FanStatus(statuses) != prose.FanOK {
			fmt.Fprintf(&body, "\nnext: %s\n", closing())
		}
	} else {
		fmt.Fprintf(&body, "fan-out: %s\n", dir)
		fmt.Fprintf(&body, "status: %s\n", prose.FanStatusLine(statuses))
		fmt.Fprintf(&body, "members: %s\n", strings.Join(labels, " · "))
		// The set-level follow-up is offered only when it is provably
		// possible: every member finished and holds a session to continue.
		if resumable {
			fmt.Fprintf(&body, "resume: %s\n", prose.ContinueCommand(dir, group.TimeoutMin))
		}
		// Every member reviewed the same range, so it is reported once here
		// rather than repeated under each of them.
		if group.GitBaseline != nil {
			printGitSinceBaseline(&body, group.Cwd, *group.GitBaseline)
		}
		body.Write(sections.Bytes())
		if mode == ModeStatusOnly {
			fmt.Fprintf(&body, "\nnext: %s\n", prose.StatusOnlyNext(dir))
		} else {
			fmt.Fprintf(&body, "\nnext: %s\n", closing())
		}
	}
	if !deliver(w, errW, withNote(body.Bytes(), note, mode, errW)) {
		return job.ExitInfra
	}
	for _, s := range stamps {
		stampCollected(s.dir, s.meta)
	}
	return 0
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

// ---------- pending discovery ----------

// pendingItem is one job needing attention after a possibly missed
// notification.
type pendingItem struct {
	kind   string // runningState kinds, plus "corrupt"
	dir    string
	detail string
	meta   *job.Meta
}

func pendingJobs(dirs []string) []pendingItem {
	var pending []pendingItem
	for _, dir := range dirs {
		var item pendingItem
		var ok bool
		if job.IsGroupDir(dir) {
			item, ok = pendingGroup(dir)
		} else {
			meta, err := job.ReadMeta(dir)
			item, ok = pendingTurn(dir, meta, err)
		}
		if ok {
			pending = append(pending, item)
		}
	}
	return pending
}

// pendingTurn classifies one turn record, as read: still needing attention,
// or not.
func pendingTurn(dir string, meta *job.Meta, err error) (pendingItem, bool) {
	if errors.Is(err, job.ErrNoRecord) {
		return pendingItem{}, false
	}
	if err != nil {
		return pendingItem{
			kind: "corrupt", dir: dir,
			detail: fmt.Sprintf("meta.json could not be read: %s", err),
		}, true
	}
	if meta.Status == job.StatusRunning {
		state := classifyRunning(meta)
		if state.kind == "live" {
			return pendingItem{}, false
		}
		return pendingItem{kind: state.kind, dir: dir, detail: state.detail, meta: meta}, true
	}
	if meta.CollectedAt == nil {
		return pendingItem{
			kind: "terminal", dir: dir, meta: meta,
			detail: fmt.Sprintf("terminal status %s has not been collected", meta.Status),
		}, true
	}
	return pendingItem{}, false
}

// pendingGroup rolls a fan-out up as the single entry a caller acts on: one
// collect covers every member. Discovery names which members need attention and
// why; the prescription for each of them is per member, and printing it is
// collect's job, not the index's.
func pendingGroup(dir string) (pendingItem, bool) {
	fan, err := job.ReadFan(dir)
	if err != nil {
		return pendingItem{
			kind: "corrupt", dir: dir,
			detail: fmt.Sprintf("group.json could not be read: %s", err),
		}, true
	}
	var reasons []string
	for _, m := range fan.Members {
		// A member the roster names but that never wrote a record is the one
		// case the member's own files cannot report; the roster is the only
		// evidence it was meant to run, so the group carries it.
		if errors.Is(m.Err, job.ErrNoRecord) {
			reasons = append(reasons, fmt.Sprintf("%s: has no meta.json, so it never recorded a start", m.Name))
			continue
		}
		if item, ok := pendingTurn(m.Dir, m.Meta, m.Err); ok {
			reasons = append(reasons, fmt.Sprintf("%s: %s", m.Name, item.detail))
		}
	}
	if len(reasons) == 0 {
		return pendingItem{}, false
	}
	return pendingItem{
		kind: "group", dir: dir,
		detail: fmt.Sprintf("%d of %d members still need attention — %s",
			len(reasons), len(fan.Members), strings.Join(reasons, " · ")),
	}, true
}

// Pending prints the discovery-only recovery index: it skips provably live
// jobs, does not print full results, and does not mark anything collected.
func Pending(base string, baseWasDerived bool, w, errW io.Writer) int {
	// "no recovery action is needed" over a store that could not be read is the
	// most reassuring thing this command can say and the least earned. The one
	// benign error is a derived store that does not exist yet — a project
	// before its first dispatch creates it. The same error on a --base the
	// caller typed means it named something that isn't there.
	dirs, err := jobDirs(base)
	firstRun := baseWasDerived && errors.Is(err, fs.ErrNotExist)
	if err != nil && !firstRun {
		fmt.Fprintf(errW, "pending error: %s\n", prose.UnreadableStore(base, err))
		return job.ExitInfra
	}
	pending := pendingJobs(dirs)
	fmt.Fprintf(w, "pending jobs: %d (under %s)\n", len(pending), base)
	if len(pending) == 0 {
		fmt.Fprintln(w, "next: no recovery action is needed")
		return 0
	}
	for _, item := range pending {
		head := item.kind
		if item.kind == "terminal" && item.meta != nil {
			head += ":" + item.meta.Status
		}
		fmt.Fprintf(w, "\n[%s] %s\n", head, item.dir)
		fmt.Fprintf(w, "why: %s\n", item.detail)
		ws := job.Workspace{Dir: item.dir}
		switch item.kind {
		case "terminal", "group", "abandoned":
			// Collection reconciles an abandoned record and renders its
			// recovery from the reconciled state; the index only points there.
			fmt.Fprintf(w, "next: %s\n", prose.CollectCommand(item.dir))
		case "corrupt":
			fmt.Fprintf(w, "next: inspect %s, %s, %s, and the working tree; do not infer completion from the damaged metadata\n",
				ws.ProgressLogPath(), ws.RawLogPath(), ws.StderrLogPath())
		default:
			fmt.Fprintf(w, "next: %s\n", recoveryForStale(item.dir, item.meta, runningState{kind: item.kind, detail: item.detail}))
		}
	}
	return 0
}

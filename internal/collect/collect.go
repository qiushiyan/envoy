// Package collect recovers or prints one finished (or stranded) job as a
// single scannable block, and discovers pending jobs after a possibly missed
// completion notification. Explicit collection stamps terminal jobs with
// collectedAt; pending discovery never marks anything collected.
package collect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/gitx"
	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/steer"
)

// JobDirs lists job directories under base, oldest first (stamped names make
// lexical order chronological). A dir counts as a job once prompt.md exists.
func JobDirs(base string) []string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "prompt.md")); err == nil {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// LatestJobDir returns the newest job under base, or "" when there is none.
func LatestJobDir(base string) string {
	dirs := JobDirs(base)
	if len(dirs) == 0 {
		return ""
	}
	return dirs[len(dirs)-1]
}

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

// resumeCommand rebuilds this job's follow-up command from what the turn was
// dispatched with, so the caller never has to assemble one — or discover too
// late that the original turn's write intent was dropped.
func resumeCommand(meta *job.Meta) string {
	if meta.SessionLockConflict != nil || meta.SessionID == nil {
		return ""
	}
	return steer.Turn{
		Provider:   meta.Provider,
		SessionID:  *meta.SessionID,
		Cwd:        meta.Cwd,
		Model:      deref(meta.Model),
		Effort:     deref(meta.Effort),
		AllowWrite: meta.AllowWrite,
		TimeoutMin: meta.TimeoutMin,
	}.ResumeCommand()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// recoveryForStale prescribes the next move for a job whose runner is gone.
// A live orphan provider outranks the prompt state: acting alongside it would
// put two turns in one tree.
func recoveryForStale(meta *job.Meta, state runningState) string {
	if meta.SessionLockConflict != nil {
		return steer.LockedSession(*meta.SessionLockConflict, state.kind == "orphaned")
	}
	switch state.kind {
	case "orphaned":
		return steer.Orphaned()
	case "abandoned":
		return steer.Recovery(meta.PromptState, resumeCommand(meta), "")
	default:
		return steer.Unprovable()
	}
}

// reconcileAbandoned rewrites a provably abandoned running job as terminal
// status "abandoned" so its evidence survives and its next action is recorded.
func reconcileAbandoned(outDir string, meta *job.Meta, state runningState) *job.Meta {
	if state.kind != "abandoned" {
		return meta
	}
	errText := fmt.Sprintf("This turn ended without publishing a result: %s.", state.detail)
	meta.Status = job.StatusAbandoned
	meta.ReconciledAt = job.Ptr(job.ISO(time.Now()))
	meta.Error = job.Ptr(errText)
	meta.NextAction = steer.CollectThisJob(outDir)
	meta.RecoveryAction = job.Ptr(recoveryForStale(meta, state))
	resultPath := filepath.Join(outDir, "result.md")
	if _, err := os.Stat(resultPath); os.IsNotExist(err) {
		job.WriteFileAtomic(resultPath, []byte(fmt.Sprintf("# Turn abandoned\n\n%s\n", errText)))
	}
	meta.WriteFile(filepath.Join(outDir, "meta.json"))
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
	ModeStatusOnly
)

// Collect prints one job — a single turn, or a whole fan-out with a section per
// member — and stamps first terminal collection. Returns a process exit code.
func Collect(outDir string, mode Mode, w, errW io.Writer) int {
	if job.IsGroupDir(outDir) {
		return collectGroup(outDir, mode, w, errW)
	}
	_, code := collectJob(outDir, mode, w, errW, true)
	return code
}

// collectJob prints one turn and reports the meta it published, so a fan-out
// can aggregate its members without a second reader of meta.json. showGit is
// false for a member of a fan-out, where the reviewed range belongs to the
// whole fan-out and is printed once above the members rather than per member.
func collectJob(outDir string, mode Mode, w, errW io.Writer, showGit bool) (collected *job.Meta, code int) {
	metaPath := filepath.Join(outDir, "meta.json")
	resultPath := filepath.Join(outDir, "result.md")
	if _, err := os.Stat(metaPath); err != nil {
		fmt.Fprintf(errW,
			"collect error: %s not found — inspect %s, %s, %s, and the working tree before deciding whether to retry\n",
			metaPath,
			filepath.Join(outDir, "progress.log"),
			filepath.Join(outDir, "raw.log"),
			filepath.Join(outDir, "stderr.log"))
		return nil, job.ExitUsage
	}
	meta, _, err := job.ReadMetaFile(metaPath)
	if err != nil {
		fmt.Fprintf(errW,
			"collect error: %s is not valid JSON (%s). Inspect %s, %s, %s, and the working tree; do not assume the job finished or retry it blindly.\n",
			metaPath, err,
			filepath.Join(outDir, "progress.log"),
			filepath.Join(outDir, "raw.log"),
			filepath.Join(outDir, "stderr.log"))
		return nil, job.ExitUsage
	}
	state := classifyRunning(meta)
	meta = reconcileAbandoned(outDir, meta, state)
	state = classifyRunning(meta)

	if mode == ModeResultOnly && meta.Status == job.StatusOK {
		if data, err := os.ReadFile(resultPath); err == nil {
			fmt.Fprintln(w, string(data))
		} else {
			fmt.Fprintln(w, "(no result.md yet)")
		}
		stampCollected(metaPath, meta, steer.CollectedOK())
		return meta, 0
	}

	fmt.Fprintf(w, "job: %s\n", outDir)
	if meta.Status == job.StatusRunning {
		fmt.Fprintf(w, "status: running (%s — %s; result.md is not final)\n", state.kind, state.detail)
	} else {
		fmt.Fprintf(w, "status: %s\n", steer.StatusLine(meta.Status))
	}
	display := func(v *string) string {
		if v == nil {
			return "(provider default)"
		}
		return *v
	}
	labelSuffix := ""
	if meta.Label != nil {
		labelSuffix = " · label " + *meta.Label
	}
	// The model column shows the request; when the provider announced what it
	// actually resolved, show that observation too.
	modelDisplay := display(meta.Model)
	if meta.ProviderReportedModel != nil {
		reported := *meta.ProviderReportedModel
		switch {
		case meta.Model == nil:
			modelDisplay = fmt.Sprintf("(provider default, ran %s)", reported)
		case *meta.Model != reported:
			modelDisplay = fmt.Sprintf("%s (ran %s)", *meta.Model, reported)
		}
	}
	fmt.Fprintf(w, "provider: %s · model %s · effort %s%s\n", meta.Provider, modelDisplay, display(meta.Effort), labelSuffix)
	if meta.DurationMs != nil {
		costSuffix := ""
		if meta.CostUSD != nil {
			costSuffix = fmt.Sprintf(" · cost $%g", *meta.CostUSD)
		}
		fmt.Fprintf(w, "duration: %dm%s\n", int64(float64(*meta.DurationMs)/60000+0.5), costSuffix)
	}
	tokens := "n/a"
	if pairs := meta.Tokens.Pairs(); len(pairs) > 0 {
		var parts []string
		for _, p := range pairs {
			parts = append(parts, fmt.Sprintf("%s %s", p[0], groupThousands(p[1].(int64))))
		}
		tokens = strings.Join(parts, " · ")
	}
	fmt.Fprintf(w, "tokens: %s\n", tokens)
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
	if meta.Status == job.StatusRunning && meta.WatchCommand != "" {
		fmt.Fprintf(w, "watch: %s\n", meta.WatchCommand)
	}
	fmt.Fprintf(w, "logs: progress %s · raw %s · stderr %s\n",
		orDefault(meta.ProgressPath, filepath.Join(outDir, "progress.log")),
		orDefault(meta.RawPath, filepath.Join(outDir, "raw.log")),
		orDefault(meta.StderrPath, filepath.Join(outDir, "stderr.log")))
	if meta.SessionID != nil {
		fmt.Fprintf(w, "session: %s\n", *meta.SessionID)
		if resume := resumeCommand(meta); resume != "" {
			fmt.Fprintf(w, "resume: %s\n", resume)
		}
		if meta.TakeoverCommand != nil {
			fmt.Fprintf(w, "takeover: %s\n", *meta.TakeoverCommand)
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

	if mode != ModeStatusOnly {
		fmt.Fprintln(w, "\n--- result.md ---")
		if data, err := os.ReadFile(resultPath); err == nil {
			fmt.Fprintln(w, string(data))
		} else {
			fmt.Fprintln(w, "(no result.md yet)")
		}
	}

	if meta.Status == job.StatusRunning {
		if state.kind == "live" {
			fmt.Fprintf(w, "\nnext: %s\n", steer.RunningNext(outDir, meta.WatchCommand))
		} else {
			fmt.Fprintf(w, "\nnext: %s\n", recoveryForStale(meta, state))
		}
		return meta, 0
	}

	if mode == ModeStatusOnly {
		fmt.Fprintf(w, "\nnext: %s\n", steer.StatusOnlyNext(outDir))
		return meta, 0
	}

	postCollectionAction := ""
	if meta.Status == job.StatusOK {
		postCollectionAction = steer.CollectedOK()
	} else if meta.RecoveryAction != nil {
		postCollectionAction = *meta.RecoveryAction
	} else {
		postCollectionAction = meta.NextAction
	}
	stampCollected(metaPath, meta, postCollectionAction)
	fmt.Fprintf(w, "\nnext: %s\n", postCollectionAction)
	return meta, 0
}

// stampCollected marks first terminal collection: the result body has actually
// been delivered to a caller, so pending discovery stops listing the job.
func stampCollected(metaPath string, meta *job.Meta, nextAction string) {
	if meta.CollectedAt != nil {
		return
	}
	meta.CollectedAt = job.Ptr(job.ISO(time.Now()))
	meta.NextAction = nextAction
	meta.WriteFile(metaPath)
}

// collectGroup prints a whole fan-out: the aggregate first, then one section
// per member, split by member name. The member sections are rendered before
// the header is written because collecting a member can change its status —
// reconciling an abandoned turn — and an aggregate that disagreed with the
// sections below it would be worse than no aggregate at all.
func collectGroup(dir string, mode Mode, w, errW io.Writer) int {
	gw := job.GroupWorkspace{Dir: dir}
	group, err := job.ReadGroupFile(gw.GroupPath())
	if err != nil {
		fmt.Fprintf(errW,
			"collect error: %s is unreadable (%s). Each member's job dir under %s is self-contained — collect one directly to see its result.\n",
			gw.GroupPath(), err, dir)
		return job.ExitUsage
	}

	var body bytes.Buffer
	statuses := make([]string, 0, len(group.Members))
	labels := make([]string, 0, len(group.Members))
	resumable := len(group.Members) > 0
	for _, m := range group.Members {
		fmt.Fprintf(&body, "\n=== member %s ===\n", m.Name)
		meta, code := collectJob(m.OutDir, mode, &body, errW, false)
		status := ""
		if code != 0 {
			// The member dir carries no readable meta: the turn never got far
			// enough to publish one. collectJob has already said so on stderr.
			fmt.Fprintf(&body, "status: %s\n", steer.FanUndispatched())
		} else {
			status = meta.Status
		}
		if meta == nil || meta.Status == job.StatusRunning || meta.SessionID == nil {
			resumable = false
		}
		statuses = append(statuses, status)
		label := status
		if label == "" {
			label = "no status"
		}
		labels = append(labels, m.Name+" "+label)
	}

	// Result-only keeps the aggregate line and the member split — attribution
	// is the point of a fan-out — and drops the coordinate preamble.
	if mode == ModeResultOnly {
		fmt.Fprintf(w, "status: %s\n", steer.FanStatusLine(statuses))
		w.Write(body.Bytes())
		if steer.FanStatus(statuses) != steer.FanOK {
			fmt.Fprintf(w, "\nnext: %s\n", steer.FanCollected(dir, statuses))
		}
		return 0
	}

	fmt.Fprintf(w, "fan-out: %s\n", dir)
	fmt.Fprintf(w, "status: %s\n", steer.FanStatusLine(statuses))
	fmt.Fprintf(w, "members: %s\n", strings.Join(labels, " · "))
	fmt.Fprintf(w, "prompt: %s\n", gw.PromptPath())
	if group.ResumedFrom != nil {
		fmt.Fprintf(w, "resumed-from: %s\n", *group.ResumedFrom)
	}
	// The set-level follow-up is offered only when it is provably possible:
	// every member finished and holds a session to continue.
	if resumable {
		fmt.Fprintf(w, "resume: %s\n", steer.FanResumeCommand(dir, group.TimeoutMin))
	}
	if group.EndedAt == nil {
		fmt.Fprintf(w, "watch: %s\n", group.WatchCommand)
	}
	// Every member reviewed the same range, so it is reported once here rather
	// than repeated under each of them.
	if group.GitBaseline != nil {
		printGitSinceBaseline(w, group.Cwd, *group.GitBaseline)
	}
	w.Write(body.Bytes())
	if mode == ModeStatusOnly {
		fmt.Fprintf(w, "\nnext: %s\n", steer.StatusOnlyNext(dir))
	} else {
		fmt.Fprintf(w, "\nnext: %s\n", steer.FanCollected(dir, statuses))
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

// pendingItem is one job needing attention after a possibly missed
// notification.
type pendingItem struct {
	kind   string // runningState kinds, plus "corrupt"
	dir    string
	detail string
	meta   *job.Meta
}

func pendingJobs(base string) []pendingItem {
	var pending []pendingItem
	for _, dir := range JobDirs(base) {
		if job.IsGroupDir(dir) {
			if item, ok := pendingGroup(dir); ok {
				pending = append(pending, item)
			}
			continue
		}
		if item, ok := pendingJob(dir); ok {
			pending = append(pending, item)
		}
	}
	return pending
}

// pendingJob classifies one turn: still needing attention, or not.
func pendingJob(dir string) (pendingItem, bool) {
	metaPath := filepath.Join(dir, "meta.json")
	if _, err := os.Stat(metaPath); err != nil {
		return pendingItem{}, false
	}
	meta, raw, err := job.ReadMetaFile(metaPath)
	if err != nil {
		return pendingItem{
			kind: "corrupt", dir: dir,
			detail: fmt.Sprintf("meta.json is unreadable: %s", err),
		}, true
	}
	if meta.Status == job.StatusRunning {
		state := classifyRunning(meta)
		if state.kind == "live" {
			return pendingItem{}, false
		}
		return pendingItem{kind: state.kind, dir: dir, detail: state.detail, meta: meta}, true
	}
	if hasNullCollectedAt(raw) {
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
	group, err := job.ReadGroupFile(job.GroupWorkspace{Dir: dir}.GroupPath())
	if err != nil {
		return pendingItem{
			kind: "corrupt", dir: dir,
			detail: fmt.Sprintf("group.json is unreadable: %s", err),
		}, true
	}
	var reasons []string
	for _, m := range group.Members {
		if item, ok := pendingJob(m.OutDir); ok {
			reasons = append(reasons, fmt.Sprintf("%s: %s", m.Name, item.detail))
		}
	}
	if len(reasons) == 0 {
		return pendingItem{}, false
	}
	return pendingItem{
		kind: "group", dir: dir,
		detail: fmt.Sprintf("%d of %d members still need attention — %s",
			len(reasons), len(group.Members), strings.Join(reasons, " · ")),
	}, true
}

func hasNullCollectedAt(raw map[string]json.RawMessage) bool {
	v, ok := raw["collectedAt"]
	return ok && string(v) == "null"
}

// Pending prints the discovery-only recovery index: it skips provably live
// jobs, does not print full results, and does not mark anything collected.
func Pending(base string, w io.Writer) int {
	pending := pendingJobs(base)
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
		switch item.kind {
		case "terminal", "group":
			fmt.Fprintf(w, "next: %s\n", steer.CollectCommand(item.dir))
		case "corrupt":
			fmt.Fprintf(w, "next: inspect %s, %s, %s, and the working tree; do not infer completion from the damaged metadata\n",
				filepath.Join(item.dir, "progress.log"),
				filepath.Join(item.dir, "raw.log"),
				filepath.Join(item.dir, "stderr.log"))
		default:
			fmt.Fprintf(w, "next: %s\n", recoveryForStale(item.meta, runningState{kind: item.kind, detail: item.detail}))
		}
	}
	return 0
}

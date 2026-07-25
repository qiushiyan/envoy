// Package collect recovers or prints one finished (or stranded) job as a
// single scannable block, and discovers pending jobs after a possibly missed
// completion notification. Explicit collection stamps terminal jobs with
// collectedAt; pending discovery never marks anything collected.
package collect

import (
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
	"github.com/qiushiyan/envoy/internal/text"
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

// resumeArgs reconstructs the resume fragment, always carrying a --timeout-min
// so a recovery turn does not silently fall back to the engine default.
func resumeArgs(meta *job.Meta) string {
	if meta.SessionLockConflict != nil {
		return ""
	}
	base := ""
	switch {
	case meta.ResumeArgs != nil:
		base = *meta.ResumeArgs
	case meta.ResumeFlag != nil:
		base = *meta.ResumeFlag
	case meta.SessionID != nil:
		base = "--resume " + *meta.SessionID
	}
	if base == "" || strings.Contains(base, "--timeout-min") {
		return base
	}
	timeoutMin := meta.TimeoutMin
	if timeoutMin < 0 {
		timeoutMin = 30
	}
	return fmt.Sprintf("%s --timeout-min %g", base, timeoutMin)
}

// recoveryForStale prescribes the next move for a job whose runner is gone,
// from observed prompt state only. Never redispatch merely because output was
// quiet.
func recoveryForStale(meta *job.Meta, state runningState) string {
	if meta.SessionLockConflict != nil {
		conflict := strings.TrimRight(*meta.SessionLockConflict, ". \t\n")
		providerWarning := ""
		if state.kind == "orphaned" {
			providerWarning = " This job also has a provider process that may still be alive; stop or wait for it first."
		}
		return fmt.Sprintf(
			"This turn recorded a session-lock collision: %s.%s Inspect or collect the job named in that conflict. Do not resume or redispatch the locked session from this job.",
			conflict, providerWarning)
	}
	switch state.kind {
	case "orphaned":
		return "Another provider process may still be changing the tree. Stop or wait for that process first, then inspect progress.log, raw.log, stderr.log, and the working tree. Resume or redispatch only after the process is gone."
	case "abandoned":
		if meta.PromptState == job.PromptAccepted && meta.SessionID != nil {
			return fmt.Sprintf(
				"Inspect the recovered result, progress.log, raw.log, stderr.log, and the working tree, then continue the same session with %s; do not redispatch the original prompt because it could duplicate accepted work.",
				resumeArgs(meta))
		}
		if meta.PromptState == job.PromptNotStarted {
			return "The provider process did not start, so the prompt was not accepted. Retry the identical dispatch once."
		}
		tail := ""
		if meta.SessionID != nil {
			tail = fmt.Sprintf("; otherwise continue with %s", resumeArgs(meta))
		}
		return fmt.Sprintf(
			"Prompt acceptance is unconfirmed; absence of output is not proof that no work occurred. Inspect progress.log, raw.log, stderr.log, and the working tree. Redispatch only if you can positively establish that the prompt never began%s.",
			tail)
	default:
		return "Inspect progress.log, raw.log, stderr.log, the provider process list, and the working tree before resuming or redispatching; liveness is unknown."
	}
}

// reconcileAbandoned rewrites a provably abandoned running job as terminal
// status "abandoned" so its evidence survives and its next action is recorded.
func reconcileAbandoned(outDir string, meta *job.Meta, state runningState) *job.Meta {
	if state.kind != "abandoned" {
		return meta
	}
	errText := fmt.Sprintf("The envoy runner ended without publishing a terminal result; %s.", state.detail)
	meta.Status = job.StatusAbandoned
	meta.ReconciledAt = job.Ptr(job.ISO(time.Now()))
	meta.Error = job.Ptr(errText)
	meta.NextAction = fmt.Sprintf("Collect and inspect this reconciled job: envoy collect %s.", text.ShellQuote(outDir))
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

// Collect prints one job's block and stamps first terminal collection.
// Returns a process exit code.
func Collect(outDir string, w, errW io.Writer) int {
	metaPath := filepath.Join(outDir, "meta.json")
	resultPath := filepath.Join(outDir, "result.md")
	if _, err := os.Stat(metaPath); err != nil {
		fmt.Fprintf(errW,
			"collect error: %s not found — inspect %s, %s, %s, and the working tree before deciding whether to retry\n",
			metaPath,
			filepath.Join(outDir, "progress.log"),
			filepath.Join(outDir, "raw.log"),
			filepath.Join(outDir, "stderr.log"))
		return job.ExitUsage
	}
	meta, _, err := job.ReadMetaFile(metaPath)
	if err != nil {
		fmt.Fprintf(errW,
			"collect error: %s is not valid JSON (%s). Inspect %s, %s, %s, and the working tree; do not assume the job finished or retry it blindly.\n",
			metaPath, err,
			filepath.Join(outDir, "progress.log"),
			filepath.Join(outDir, "raw.log"),
			filepath.Join(outDir, "stderr.log"))
		return job.ExitUsage
	}
	state := classifyRunning(meta)
	meta = reconcileAbandoned(outDir, meta, state)
	state = classifyRunning(meta)

	fmt.Fprintf(w, "job: %s\n", outDir)
	if meta.Status == job.StatusRunning {
		fmt.Fprintf(w, "status: running (%s — %s; result.md is not final)\n", state.kind, state.detail)
	} else {
		fmt.Fprintf(w, "status: %s\n", meta.Status)
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
	fmt.Fprintf(w, "provider: %s · model %s · effort %s%s\n", meta.Provider, display(meta.Model), display(meta.Effort), labelSuffix)
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
		if resume := resumeArgs(meta); resume != "" {
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

	if meta.GitBaseline != nil {
		baseline := *meta.GitBaseline
		fmt.Fprintf(w, "\n--- git since baseline %s (in %s) ---\n", baseline, meta.Cwd)
		fmt.Fprintln(w, orDefault(gitx.Run(meta.Cwd, "log", baseline+"..HEAD", "--oneline"), "(no commits)"))
		fmt.Fprintln(w, orDefault(gitx.Run(meta.Cwd, "diff", baseline, "--stat"), "(no diff)"))
		if dirty := gitx.Run(meta.Cwd, "status", "--short"); dirty != "" {
			fmt.Fprintf(w, "dirty:\n%s\n", dirty)
		} else {
			fmt.Fprintln(w, "tree clean")
		}
	}

	fmt.Fprintln(w, "\n--- result.md ---")
	if data, err := os.ReadFile(resultPath); err == nil {
		fmt.Fprintln(w, string(data))
	} else {
		fmt.Fprintln(w, "(no result.md yet)")
	}

	if meta.Status == job.StatusRunning {
		if state.kind == "live" {
			watchHint := ""
			if meta.WatchCommand != "" {
				watchHint = fmt.Sprintf(" Use %s only for live observation.", meta.WatchCommand)
			}
			fmt.Fprintf(w, "\nnext: Return now and wait for Claude Code's native background-task notification.%s\n", watchHint)
		} else {
			fmt.Fprintf(w, "\nnext: %s\n", recoveryForStale(meta, state))
		}
		return 0
	}

	postCollectionAction := ""
	if meta.Status == job.StatusOK {
		postCollectionAction = "Use this result in the invoking skill's verification, judgment, or synthesis step."
	} else if meta.RecoveryAction != nil {
		postCollectionAction = *meta.RecoveryAction
	} else {
		postCollectionAction = meta.NextAction
	}
	if meta.CollectedAt == nil {
		meta.CollectedAt = job.Ptr(job.ISO(time.Now()))
		meta.NextAction = postCollectionAction
		meta.WriteFile(metaPath)
	}
	fmt.Fprintf(w, "\nnext: %s\n", postCollectionAction)
	return 0
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
		metaPath := filepath.Join(dir, "meta.json")
		if _, err := os.Stat(metaPath); err != nil {
			continue
		}
		meta, raw, err := job.ReadMetaFile(metaPath)
		if err != nil {
			pending = append(pending, pendingItem{
				kind: "corrupt", dir: dir,
				detail: fmt.Sprintf("meta.json is unreadable: %s", err),
			})
			continue
		}
		if meta.Status == job.StatusRunning {
			state := classifyRunning(meta)
			if state.kind != "live" {
				pending = append(pending, pendingItem{kind: state.kind, dir: dir, detail: state.detail, meta: meta})
			}
			continue
		}
		if hasNullCollectedAt(raw) {
			pending = append(pending, pendingItem{
				kind: "terminal", dir: dir, meta: meta,
				detail: fmt.Sprintf("terminal status %s has not been collected", meta.Status),
			})
		}
	}
	return pending
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
		case "terminal":
			fmt.Fprintf(w, "next: envoy collect %s\n", text.ShellQuote(item.dir))
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

package collect

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
)

// Steer answers a steer request against one job dir. Nothing is ever
// delivered into a live turn — no provider accepts input into one — so the
// answer is always an honest "not delivered" plus the one command that does
// carry the supplement: the follow-up that continues the same session, with
// the supplement file already in its --prompt-file slot. Steer only reads the
// job: no reconciliation, no collectedAt stamp, no state change anywhere.
func Steer(outDir, promptFile string, w, errW io.Writer) int {
	if job.IsGroupDir(outDir) {
		return steerGroup(outDir, promptFile, w, errW)
	}
	metaPath := filepath.Join(outDir, "meta.json")
	if _, err := os.Stat(metaPath); err != nil {
		fmt.Fprintf(errW,
			"steer error: %s not found — pass the out-dir printed at dispatch, or a fan-out member directory\n",
			metaPath)
		return job.ExitUsage
	}
	meta, _, err := job.ReadMetaFile(metaPath)
	if err != nil {
		fmt.Fprintf(errW,
			"steer error: %s is not valid JSON (%s); collect the job before acting on it\n", metaPath, err)
		return job.ExitUsage
	}
	// The runner records a versioned schema, a known status, and a provider
	// from the first moment, so a meta missing any of them is some other
	// program's file that happens to parse — not a job to reason about. The
	// version's value is not gated, only its presence: discrimination, not
	// compatibility policy.
	if meta.SchemaVersion < 1 || !job.KnownStatus(meta.Status) || meta.Provider == "" {
		fmt.Fprintf(errW,
			"steer error: %s is not a job this engine wrote (status %q) — pass the out-dir printed at dispatch\n",
			metaPath, meta.Status)
		return job.ExitUsage
	}
	fmt.Fprint(w, steerReport(meta, classifyRunning(meta), outDir, promptFile).Block(outDir))
	return 0
}

// steerReport maps one turn's observed state onto prose's steer vocabulary.
// The follow-up command is the job's own resume shape rendered with the
// supplement in the prompt slot — the one place that placeholder closes,
// because here the new prompt already exists in the caller's hand.
func steerReport(meta *job.Meta, state runningState, outDir, promptFile string) prose.SteerReport {
	followUp := ""
	if turn, ok := turnFromMeta(meta); ok {
		followUp = turn.ResumeCommandWith(promptFile)
	}
	if meta.Status == job.StatusRunning {
		if state.kind == "live" {
			return prose.SteerLive(meta.Provider, followUp, outDir)
		}
		return prose.SteerStale(state.kind, outDir)
	}
	if meta.Status == job.StatusOK && followUp != "" {
		return prose.SteerTerminalOK(followUp, outDir, meta.CollectedAt != nil)
	}
	// Every other terminal state — and an ok turn with no usable session, such
	// as a recorded lock conflict — routes through collect, which owns the
	// recovery decision for that turn.
	return prose.SteerTerminalNotOK(meta.Status, outDir)
}

// steerGroup redirects a steer aimed at a fan-out directory to its members,
// each with its own runnable steer line — a group holds no conversation of
// its own to supplement.
func steerGroup(dir, promptFile string, w, errW io.Writer) int {
	gw := job.GroupWorkspace{Dir: dir}
	group, err := job.ReadGroupFile(gw.GroupPath())
	if err != nil {
		fmt.Fprintf(errW,
			"steer error: %s is unreadable (%s). Each member's job dir under %s is self-contained — steer one directly.\n",
			gw.GroupPath(), err, dir)
		return job.ExitUsage
	}
	if len(group.Members) == 0 {
		fmt.Fprintf(errW, "steer error: %s lists no members\n", gw.GroupPath())
		return job.ExitUsage
	}
	memberCmds := make([]string, 0, len(group.Members))
	for _, m := range group.Members {
		memberCmds = append(memberCmds, prose.SteerCommand(promptFile, m.OutDir))
	}
	round := prose.FanResumeCommandWith(dir, group.TimeoutMin, promptFile)
	fmt.Fprint(w, prose.SteerGroup(memberCmds, round).Block(dir))
	return 0
}

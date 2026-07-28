// Package envoy runs one headless AI-session turn (claude or codex) as a
// background-friendly job and returns it as durable data: result.md is the
// return value, meta.json the coordinates and recovery state, progress.log the
// live semantic view.
//
// This package is the embeddable facade over the engine; cmd/envoy is the CLI
// skin. Functions return process exit codes and write agent-facing text to the
// provided writers, because the primary caller is an agent reading stdout.
package envoy

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiushiyan/envoy/internal/collect"
	"github.com/qiushiyan/envoy/internal/fan"
	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
	"github.com/qiushiyan/envoy/internal/provider"
	"github.com/qiushiyan/envoy/internal/runner"
)

// Version of the engine, reported by `envoy version`.
const Version = "0.1.0"

// Exit codes: 0 ok · 1 provider failure · 2 infra · 3 usage · 4 timeout ·
// 5 interrupted · 6 partial (fan-out only).
const (
	ExitOK          = job.ExitOK
	ExitFailed      = job.ExitFailed
	ExitInfra       = job.ExitInfra
	ExitUsage       = job.ExitUsage
	ExitTimeout     = job.ExitTimeout
	ExitInterrupted = job.ExitInterrupted
	ExitPartial     = job.ExitPartial
)

// TurnRequest describes one turn. Zero values mean "provider default" for
// Model/Effort, a fresh session for Resume, the current directory for Cwd,
// a derived job dir for OutDir — and the engine's 30-minute safety cap for
// TimeoutMin. Running uncapped requires saying so with NoTimeout: the
// dangerous state must not be the zero value.
type TurnRequest struct {
	Provider     string
	PromptFile   string
	Model        string
	Effort       string
	Resume       string
	Baseline     string
	AllowWrite   bool
	Cwd          string
	OutDir       string
	TimeoutMin   float64 // hard wall-clock cap in minutes; 0 = the 30-minute default
	NoTimeout    bool    // explicitly disable the cap (leave TimeoutMin zero)
	MaxBudgetUSD *float64
	Label        string

	Stdout io.Writer // coordinate blocks; defaults to os.Stdout
	Stderr io.Writer // errors and warnings; defaults to os.Stderr
}

// defaultTimeoutMin is the engine's safety cap when the caller sets none.
const defaultTimeoutMin = 30

// resolveTimeout maps the request's (TimeoutMin, NoTimeout) pair onto the
// runner's single value, where 0 means "no cap".
func resolveTimeout(req TurnRequest) (float64, error) {
	if math.IsNaN(req.TimeoutMin) || math.IsInf(req.TimeoutMin, 0) || req.TimeoutMin < 0 {
		return 0, fmt.Errorf("--timeout-min must be a number >= 0 (0 = no cap)")
	}
	if req.NoTimeout {
		if req.TimeoutMin != 0 {
			return 0, fmt.Errorf("NoTimeout and a non-zero TimeoutMin are mutually exclusive")
		}
		return 0, nil
	}
	if req.TimeoutMin == 0 {
		return defaultTimeoutMin, nil
	}
	return req.TimeoutMin, nil
}

// Efforts lists the effort values a provider accepts, so callers can render
// them instead of hardcoding a copy that drifts.
func Efforts(providerName string) []string { return provider.EffortList(providerName) }

func usageError(w io.Writer, format string, args ...any) int {
	fmt.Fprintf(w, "usage error: "+format+"\n", args...)
	return ExitUsage
}

// Turn validates the request and runs one turn to terminal state.
func Turn(req TurnRequest) int {
	stdout, stderr := defaultWriters(req.Stdout, req.Stderr)

	if req.Provider == "" {
		return usageError(stderr, "--provider <claude|codex> is required")
	}
	if req.Provider != "claude" && req.Provider != "codex" {
		return usageError(stderr, "--provider must be claude or codex, got '%s'", req.Provider)
	}
	if req.PromptFile == "" {
		return usageError(stderr, "--prompt-file <path> is required")
	}
	if _, err := os.Stat(req.PromptFile); err != nil {
		return usageError(stderr, "prompt file not found: %s", req.PromptFile)
	}
	cwd := req.Cwd
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			fmt.Fprintf(stderr, "envoy: cannot determine cwd: %s\n", err)
			return ExitInfra
		}
	}
	cwd = absOrSelf(cwd)
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return usageError(stderr, "cwd not found: %s", cwd)
	}
	timeoutMin, err := resolveTimeout(req)
	if err != nil {
		return usageError(stderr, "%s", err)
	}
	if err := provider.ValidateEffort(req.Provider, req.Effort); err != nil {
		return usageError(stderr, "%s", err)
	}
	if req.MaxBudgetUSD != nil {
		if req.Provider != "claude" {
			return usageError(stderr, "--max-budget-usd exists only on claude; codex has no budget flag")
		}
		if math.IsNaN(*req.MaxBudgetUSD) || math.IsInf(*req.MaxBudgetUSD, 0) || *req.MaxBudgetUSD <= 0 {
			return usageError(stderr, "--max-budget-usd must be a positive number")
		}
	}

	outDir := req.OutDir
	if outDir != "" {
		outDir = absOrSelf(outDir)
	}
	return runner.Run(runner.Options{
		Provider:   req.Provider,
		PromptFile: req.PromptFile,
		Cwd:        cwd,
		Baseline:   req.Baseline,
		Label:      req.Label,
		OutDir:     outDir,
		Turn: provider.Options{
			Model:        req.Model,
			Effort:       req.Effort,
			Resume:       req.Resume,
			AllowWrite:   req.AllowWrite,
			TimeoutMin:   timeoutMin,
			MaxBudgetUSD: req.MaxBudgetUSD,
		},
		Stdout: stdout,
		Stderr: stderr,
	}).ExitCode
}

// FanRequest describes one fan-out: the same prompt dispatched to several
// turns, supervised as a single job. With holds one member spec per turn,
// spelled provider[:model[:effort]] — the same form a caller types — so the
// CLI and an embedding program get identical parsing and identical errors.
// ResumeFrom instead names a finished fan-out whose members this one
// continues: the roster, each member's session, the working directory, and
// the baseline all come from that fan-out's own records, and With must be
// empty.
//
// Everything else is shared by every member. There is deliberately no write
// intent and no per-member resume flag: members share one working tree, and a
// single session id cannot name several conversations. TimeoutMin follows
// TurnRequest — zero means the 30-minute cap.
type FanRequest struct {
	With       []string
	ResumeFrom string
	PromptFile string
	Baseline   string
	Cwd        string
	OutDir     string
	TimeoutMin float64
	NoTimeout  bool
	Label      string

	Stdout io.Writer
	Stderr io.Writer
}

// Fan validates the request and runs every member to terminal state. It returns
// when the last one is done: 0 when all of them returned a result, ExitPartial
// when some did, otherwise the most dispatch-side of their failures.
func Fan(req FanRequest) int {
	stdout, stderr := defaultWriters(req.Stdout, req.Stderr)

	var members []fan.Member
	resumedFrom := ""
	if req.ResumeFrom != "" {
		if len(req.With) > 0 {
			return usageError(stderr, "%s", prose.FanResumeFromAndWith())
		}
		resumedFrom = absOrSelf(req.ResumeFrom)
		resolved, group, errText := resumableMembers(resumedFrom)
		if errText != "" {
			return usageError(stderr, "%s", errText)
		}
		members = resolved
		// A resumed conversation continues in the tree and against the anchor
		// it was dispatched with; explicit flags still win.
		if req.Cwd == "" {
			req.Cwd = group.Cwd
		}
		if req.Baseline == "" && group.GitBaseline != nil {
			req.Baseline = *group.GitBaseline
		}
	} else {
		if len(req.With) < 2 {
			return usageError(stderr,
				"a fan-out needs at least two members: --with provider[:model[:effort]] --with provider[:model[:effort]]. "+
					"For one turn, use envoy turn")
		}
		members = make([]fan.Member, 0, len(req.With))
		for _, spec := range req.With {
			m, err := parseMember(spec)
			if err != nil {
				return usageError(stderr, "%s", err)
			}
			members = append(members, m)
		}
	}
	if req.PromptFile == "" {
		return usageError(stderr, "--prompt-file <path> is required")
	}
	if _, err := os.Stat(req.PromptFile); err != nil {
		return usageError(stderr, "prompt file not found: %s", req.PromptFile)
	}
	cwd := req.Cwd
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			fmt.Fprintf(stderr, "envoy: cannot determine cwd: %s\n", err)
			return ExitInfra
		}
	}
	cwd = absOrSelf(cwd)
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return usageError(stderr, "cwd not found: %s", cwd)
	}
	timeoutMin, err := resolveTimeout(TurnRequest{TimeoutMin: req.TimeoutMin, NoTimeout: req.NoTimeout})
	if err != nil {
		return usageError(stderr, "%s", err)
	}
	outDir := req.OutDir
	if outDir != "" {
		outDir = absOrSelf(outDir)
	}
	return fan.Run(fan.Options{
		Members:     members,
		PromptFile:  req.PromptFile,
		Cwd:         cwd,
		Baseline:    req.Baseline,
		Label:       req.Label,
		OutDir:      outDir,
		ResumedFrom: resumedFrom,
		TimeoutMin:  timeoutMin,
		Stdout:      stdout,
		Stderr:      stderr,
	})
}

// resumableMembers resolves a --resume-from directory into the members of a
// new round, through collect's typed inspection so the dispatch decision and
// collect's own resume line can never disagree about who can continue. The
// set is whole or refused: any blocked member refuses the round rather than
// being silently left out of it.
func resumableMembers(dir string) ([]fan.Member, *job.Group, string) {
	if !job.IsGroupDir(dir) {
		if cmd, isTurn := collect.TurnResume(dir); isTurn {
			return nil, nil, prose.FanResumeFromNotAFanOut(dir, cmd)
		}
		return nil, nil, prose.FanResumeFromNoGroup(dir)
	}
	state, err := collect.InspectFanResume(dir)
	if err != nil {
		return nil, nil, prose.FanResumeFromUnreadableManifest(dir, err)
	}
	if len(state.Members) == 0 && len(state.Blockers) == 0 {
		return nil, nil, prose.FanResumeFromEmptyManifest(dir)
	}
	if len(state.Blockers) > 0 {
		reasons := make([]string, len(state.Blockers))
		for i, b := range state.Blockers {
			reasons[i] = prose.FanResumeBlockerLine(b.Member, b.Kind, b.Detail)
		}
		return nil, nil, prose.FanResumeFromBlocked(dir, reasons)
	}
	members := make([]fan.Member, len(state.Members))
	for i, m := range state.Members {
		members[i] = fan.Member{
			Provider: m.Provider,
			Model:    m.Model,
			Effort:   m.Effort,
			Resume:   m.Session,
			Name:     m.Name,
		}
	}
	return members, state.Group, ""
}

// parseMember reads one member spec. The colon form keeps a member's settings
// unambiguously attached to that member — no provider name, model name or
// effort value contains a colon — where repeated --model/--effort flags could
// not say which member they belonged to.
func parseMember(spec string) (fan.Member, error) {
	parts := strings.Split(spec, ":")
	if len(parts) > 3 {
		return fan.Member{}, fmt.Errorf("--with '%s' has too many fields; the form is provider[:model[:effort]], for example claude:opus:high", spec)
	}
	m := fan.Member{Provider: parts[0]}
	if len(parts) > 1 {
		m.Model = parts[1]
	}
	if len(parts) > 2 {
		m.Effort = parts[2]
	}
	if m.Provider != "claude" && m.Provider != "codex" {
		return fan.Member{}, fmt.Errorf("--with '%s' must name provider claude or codex, got '%s'", spec, m.Provider)
	}
	if err := provider.ValidateEffort(m.Provider, m.Effort); err != nil {
		return fan.Member{}, fmt.Errorf("--with '%s': %s", spec, err)
	}
	return m, nil
}

// CollectRequest selects one job and which of its sections to print.
type CollectRequest struct {
	OutDir     string // "" = the newest job for the current directory's project
	ResultOnly bool   // an ok job's result body alone; a non-ok job prints its full block
	StatusOnly bool   // everything except the result body; marks nothing collected
	Stdout     io.Writer
	Stderr     io.Writer
}

// Collect prints one job (or the latest job for the current directory when
// OutDir is "") and stamps first terminal collection — except under
// StatusOnly, which delivers no result and therefore stamps nothing.
func Collect(req CollectRequest) int {
	stdout, stderr := defaultWriters(req.Stdout, req.Stderr)
	if req.ResultOnly && req.StatusOnly {
		return usageError(stderr, "--result-only and --status-only are mutually exclusive: one asks for the payload alone, the other for everything but the payload")
	}
	mode := collect.ModeFull
	switch {
	case req.ResultOnly:
		mode = collect.ModeResultOnly
	case req.StatusOnly:
		mode = collect.ModeStatusOnly
	}
	outDir := req.OutDir
	if outDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "collect error: cannot determine cwd: %s\n", err)
			return ExitUsage
		}
		base := job.DefaultBase(cwd)
		if outDir = collect.LatestJobDir(base); outDir == "" {
			fmt.Fprintf(stderr, "collect error: no job dirs under %s; pass an out-dir explicitly\n", base)
			return ExitUsage
		}
	}
	return collect.Collect(absOrSelf(outDir), mode, stdout, stderr)
}

// SteerRequest names a supplement file and the job it was meant for.
type SteerRequest struct {
	PromptFile string // the supplemental prompt, as a file — required
	OutDir     string // "" = the newest job for the current directory's project
	Stdout     io.Writer
	Stderr     io.Writer
}

// Steer answers whether a supplemental prompt can still reach a dispatched
// job. It cannot — no provider accepts input into a running turn — so the
// engine's whole job here is the honest report: why not, and the exact
// follow-up command that carries the supplement to the same session. Steer
// delivers nothing and mutates nothing.
func Steer(req SteerRequest) int {
	stdout, stderr := defaultWriters(req.Stdout, req.Stderr)
	if req.PromptFile == "" {
		return usageError(stderr, "--prompt-file <path> is required: write the supplement to a file first — it becomes the follow-up turn's prompt")
	}
	if _, err := os.Stat(req.PromptFile); err != nil {
		return usageError(stderr, "prompt file not found: %s", req.PromptFile)
	}
	outDir := req.OutDir
	if outDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "steer error: cannot determine cwd: %s\n", err)
			return ExitUsage
		}
		base := job.DefaultBase(cwd)
		if outDir = collect.LatestJobDir(base); outDir == "" {
			fmt.Fprintf(stderr, "steer error: no job dirs under %s; pass an out-dir explicitly\n", base)
			return ExitUsage
		}
	}
	// Both paths are printed into commands that may run from any directory,
	// so they must survive leaving this one.
	return collect.Steer(absOrSelf(outDir), absOrSelf(req.PromptFile), stdout, stderr)
}

// Pending prints the discovery-only recovery index for base ("" = the default
// job root for the current directory).
func Pending(base string, stdout, stderr io.Writer) int {
	stdout, stderr = defaultWriters(stdout, stderr)
	if base == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "collect error: cannot determine cwd: %s\n", err)
			return ExitUsage
		}
		base = job.DefaultBase(cwd)
	}
	return collect.Pending(absOrSelf(base), stdout)
}

func defaultWriters(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	return stdout, stderr
}

func absOrSelf(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

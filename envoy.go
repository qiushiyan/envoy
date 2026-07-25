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

	"github.com/qiushiyan/envoy/internal/collect"
	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/provider"
	"github.com/qiushiyan/envoy/internal/runner"
)

// Version of the engine, reported by `envoy version`.
const Version = "0.1.0"

// Exit codes: 0 ok · 1 provider failure · 2 infra · 3 usage · 4 timeout ·
// 5 interrupted.
const (
	ExitOK          = job.ExitOK
	ExitFailed      = job.ExitFailed
	ExitInfra       = job.ExitInfra
	ExitUsage       = job.ExitUsage
	ExitTimeout     = job.ExitTimeout
	ExitInterrupted = job.ExitInterrupted
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
	})
}

// Collect prints one job (or the latest job for the current directory when
// outDir is "") and stamps first terminal collection.
func Collect(outDir string, stdout, stderr io.Writer) int {
	stdout, stderr = defaultWriters(stdout, stderr)
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
	return collect.Collect(absOrSelf(outDir), stdout, stderr)
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

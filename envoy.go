// Package envoy runs headless AI-session turns (claude or codex) as named,
// background-friendly jobs and returns them as durable data: result.md is the
// return value, meta.json the coordinates and recovery state, progress.log
// the live semantic view.
//
// This package is the embeddable facade over the engine; cmd/envoy is the CLI
// skin. Functions return process exit codes and write agent-facing text to the
// provided writers, because the primary caller is an agent reading stdout.
package envoy

import (
	"errors"
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
const Version = "0.12.1"

// Exit codes: 0 ok · 1 provider failure · 2 infra · 3 usage · 4 timeout ·
// 5 interrupted · 6 partial (several voices only).
const (
	ExitOK          = job.ExitOK
	ExitFailed      = job.ExitFailed
	ExitInfra       = job.ExitInfra
	ExitUsage       = job.ExitUsage
	ExitTimeout     = job.ExitTimeout
	ExitInterrupted = job.ExitInterrupted
	ExitPartial     = job.ExitPartial
)

// RunRequest describes one job: a caller-chosen name (or directory), a
// prompt per voice, and one voice per turn. With holds each voice as the
// caller spells it — provider[:model[:effort]] for a cold session, or @<job>
// to continue a finished job's conversation (a fan-out reference continues
// every member and must be the only voice) — optionally followed by =<file>,
// the prompt that voice alone receives. PromptFile is the default for every
// voice without one; it may be empty when each voice carries its own. One
// voice runs as a single turn in the job directory itself; several run as a
// fan-out with a member directory each.
//
// Zero values mean the current directory for Cwd and the engine's 30-minute
// safety cap for TimeoutMin. Running uncapped requires saying so with
// NoTimeout: the dangerous state must not be the zero value. A continued
// voice inherits its provider, session, model, effort, cwd, baseline and
// write intent from the job's own records; explicit Cwd and Baseline win, and
// AllowWrite may only widen a single continued voice, never narrow it.
type RunRequest struct {
	Job        string
	With       []string
	PromptFile string
	Baseline   string
	AllowWrite bool
	Cwd        string
	TimeoutMin float64 // hard wall-clock cap in minutes; 0 = the 30-minute default
	NoTimeout  bool    // explicitly disable the cap (leave TimeoutMin zero)
	// MaxBudgetUSD caps one claude voice's spend — the safety a program
	// dispatching unattended turns needs; nil = no cap.
	MaxBudgetUSD *float64

	Stdout io.Writer // dispatch and terminal blocks; defaults to os.Stdout
	Stderr io.Writer // errors and warnings; defaults to os.Stderr
}

// defaultTimeoutMin is the engine's safety cap when the caller sets none.
const defaultTimeoutMin = 30

// resolveTimeout maps the request's (TimeoutMin, NoTimeout) pair onto the
// runner's single value, where 0 means "no cap".
func resolveTimeout(timeoutMin float64, noTimeout bool) (float64, error) {
	if math.IsNaN(timeoutMin) || math.IsInf(timeoutMin, 0) || timeoutMin < 0 {
		return 0, fmt.Errorf("--timeout-min must be a number >= 0 (0 = no cap)")
	}
	if noTimeout {
		if timeoutMin != 0 {
			return 0, fmt.Errorf("NoTimeout and a non-zero TimeoutMin are mutually exclusive")
		}
		return 0, nil
	}
	if timeoutMin == 0 {
		return defaultTimeoutMin, nil
	}
	return timeoutMin, nil
}

// Providers lists the providers the engine drives, so callers can render
// them instead of hardcoding a copy that drifts.
func Providers() []string { return provider.Names() }

// Efforts lists the effort values a provider accepts, for the same reason.
func Efforts(providerName string) []string { return provider.EffortList(providerName) }

func usageError(w io.Writer, format string, args ...any) int {
	fmt.Fprintf(w, "usage error: "+format+"\n", args...)
	return ExitUsage
}

// Run validates the request, reserves the job, and runs it to terminal state.
func Run(req RunRequest) int {
	stdout, stderr := defaultWriters(req.Stdout, req.Stderr)

	invocationCwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "envoy: cannot determine cwd: %s\n", err)
		return ExitInfra
	}
	dir, err := job.ResolveName(req.Job, invocationCwd)
	if err != nil {
		return usageError(stderr, "%s", err)
	}
	if len(req.With) == 0 {
		return usageError(stderr, "%s", prose.RunNeedsVoice())
	}
	// A default the caller named must read even if every voice overrides it:
	// a mistyped path is a mistake whatever the roster does with it.
	if req.PromptFile != "" {
		if err := promptReadable(req.PromptFile); err != nil {
			return usageError(stderr, "prompt file %s", err)
		}
	}
	timeoutMin, err := resolveTimeout(req.TimeoutMin, req.NoTimeout)
	if err != nil {
		return usageError(stderr, "%s", err)
	}

	caller := job.CallerFromEnv()
	turns, errText := resolveTurns(req, invocationCwd, caller, timeoutMin)
	if errText != "" {
		return usageError(stderr, "%s", errText)
	}
	// Every turn's prompt is checked before the name is reserved, so a
	// member's unreadable file refuses the whole dispatch rather than
	// starting its siblings.
	for _, t := range turns {
		if err := promptReadable(t.Options.PromptFile); err != nil {
			return usageError(stderr, "prompt file %s", err)
		}
	}
	if req.MaxBudgetUSD != nil {
		if len(turns) != 1 || !provider.SpendCap(turns[0].Options.Provider) {
			return usageError(stderr, "%s", spendCapRefusal())
		}
		if math.IsNaN(*req.MaxBudgetUSD) || math.IsInf(*req.MaxBudgetUSD, 0) || *req.MaxBudgetUSD <= 0 {
			return usageError(stderr, "--max-budget-usd must be a positive number")
		}
	}
	cwd := turns[0].Options.Cwd
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return usageError(stderr, "cwd not found: %s", cwd)
	}

	// The name is reserved last, after every refusal that needs no
	// reservation, and released again on any refusal that happens before the
	// job wrote its first record.
	dir, refusal, err := reserve(req.Job, dir, caller)
	if refusal != "" {
		return usageError(stderr, "%s", refusal)
	}
	if msg, ok := unresolvable(err); ok {
		fmt.Fprintf(stderr, "envoy: %s\n", msg)
		return ExitInfra
	}
	if err != nil {
		fmt.Fprintf(stderr, "envoy: cannot create job dir: %s\n", err)
		return ExitInfra
	}

	// Cardinality decides layout only: one voice runs flat in the job
	// directory, several run as a fan-out with a member directory each.
	if len(turns) == 1 {
		opts := turns[0].Options
		opts.OutDir = dir
		opts.Stdout, opts.Stderr = stdout, stderr
		code := runner.Run(opts).ExitCode
		releaseIfUnstarted(dir)
		return code
	}
	code := fan.Run(fan.Options{Turns: turns, OutDir: dir, Stdout: stdout, Stderr: stderr})
	releaseIfUnstarted(dir)
	return code
}

// unresolvable words the two ways a name can fail to resolve that are the
// store's fault rather than the caller's: both stop with exit 2 instead of
// letting an older job answer to the name.
func unresolvable(err error) (string, bool) {
	var unreadable *job.StoreUnreadableError
	var unattributed *job.UnattributedError
	switch {
	case errors.As(err, &unreadable):
		return prose.UnreadableStore(unreadable.Base, unreadable.Err), true
	case errors.As(err, &unattributed):
		return prose.UnattributedGeneration(unattributed.Name, unattributed.Dir, unattributed.Err), true
	}
	return "", false
}

// reserve claims the directory this dispatch runs in. A path is an identity
// and is taken or refused as given. A name gets a generation of its own,
// always the next one, and from then on means that job to this caller.
//
// What can still refuse it is a job this dispatch would take the name away
// from while a caller may be waiting to collect by it: the job the name means
// to this caller now, and the newest one, which is what the name means to
// every caller without a generation of its own. Such a job holds the name
// only against a caller it shares that meaning with — the same caller, or
// either side having no identity. Two callers that both have one never
// contend: each name keeps meaning each caller's own job. Losing the Mkdir to
// a concurrent dispatch re-reads the name.
func reserve(arg, dir, caller string) (reserved, refusal string, err error) {
	if job.IsPath(arg) {
		if err := job.Reserve(dir); errors.Is(err, job.ErrJobExists) {
			return "", prose.JobExists(dir), nil
		} else if err != nil {
			return "", "", err
		}
		return dir, "", nil
	}
	base, name := filepath.Dir(dir), filepath.Base(dir)
	for range 8 {
		addr, rerr := job.Resolve(base, name, caller)
		if rerr != nil {
			return "", "", rerr
		}
		if addr.Newest > 0 {
			for _, holder := range []string{addr.Dir, addr.NewestDir} {
				hold, held := collect.NameHold(holder)
				if !held {
					continue
				}
				// A holder whose record cannot say whose it is binds like one
				// with no identity: NameHold has already called it unreadable.
				if owner, _ := job.CallerOf(holder); caller == "" || owner == "" || owner == caller {
					return "", prose.NameHeld(name, holder, hold), nil
				}
			}
		}
		next := job.GenerationDir(base, name, addr.Newest+1)
		if err = job.Reserve(next); err == nil {
			return next, "", nil
		} else if !errors.Is(err, job.ErrJobExists) {
			return "", "", err
		}
	}
	return "", "", err
}

// spendCapRefusal says which voices --max-budget-usd can cap.
func spendCapRefusal() string {
	var capped, uncapped []string
	for _, name := range provider.Names() {
		if provider.SpendCap(name) {
			capped = append(capped, name)
		} else {
			uncapped = append(uncapped, name)
		}
	}
	return fmt.Sprintf("--max-budget-usd caps one %s voice; %s has no budget flag",
		strings.Join(capped, " or "), strings.Join(uncapped, " or "))
}

// promptReadable reports why a prompt file cannot be sent: missing, a
// directory, or unreadable. Existence alone is not enough — the runner reads
// the file whole, and a directory would only fail after the name was taken.
func promptReadable(path string) error {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return fmt.Errorf("not found: %s", path)
	case info.IsDir():
		return fmt.Errorf("is a directory, not a file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot be read: %s", err)
	}
	f.Close()
	return nil
}

// releaseIfUnstarted gives a reserved name back when nothing ran under it: a
// refusal after reservation (a held session, an unreadable prompt) leaves no
// record, and an occupied name with nothing to collect would be invisible.
// Once a record exists the directory is evidence and stays.
func releaseIfUnstarted(dir string) {
	if !job.HasRecord(dir) {
		os.RemoveAll(dir)
	}
}

// CollectRequest selects one job and which of its sections to print.
type CollectRequest struct {
	Job        string // the job's name in this project's store, or its directory
	ResultOnly bool   // an ok job's result body alone; a non-ok job prints its full block
	StatusOnly bool   // everything except the result body; marks nothing collected
	Stdout     io.Writer
	Stderr     io.Writer
}

// Collect prints one job and stamps first terminal collection once the block
// has reached Stdout — except under StatusOnly, which delivers no result and
// therefore stamps nothing.
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
	invocationCwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "collect error: cannot determine cwd: %s\n", err)
		return ExitInfra
	}
	if req.Job == "" {
		return usageError(stderr, "collect takes the job to print: the name it was run as, or its directory")
	}
	ref, err := job.ResolveRef(req.Job, invocationCwd, job.CallerFromEnv())
	if msg, ok := unresolvable(err); ok {
		fmt.Fprintf(stderr, "collect error: %s\n", msg)
		return ExitInfra
	}
	if err != nil {
		return usageError(stderr, "%s", err)
	}
	// A name that did not mean a job of the caller's own says so: the engine
	// cannot tell work picked up on purpose from a caller whose identity
	// changed, so it reports which happened and leaves the judgment.
	note := ""
	if ref.FellBack {
		note = prose.NameFellBack(ref.Name, ref.Owner != "")
	}
	return collect.Collect(ref.Dir, note, mode, stdout, stderr)
}

// Pending prints the discovery-only recovery index for base ("" = the default
// job root for the current directory).
func Pending(base string, stdout, stderr io.Writer) int {
	stdout, stderr = defaultWriters(stdout, stderr)
	derived := base == ""
	if base == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "collect error: cannot determine cwd: %s\n", err)
			return ExitUsage
		}
		base = job.DefaultBase(cwd)
	}
	return collect.Pending(absOrSelf(base), derived, stdout, stderr)
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

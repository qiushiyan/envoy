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
const Version = "0.7.0"

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

// RunRequest describes one job: a caller-chosen name (or directory), one
// prompt, and one voice per turn. With holds each voice as the caller spells
// it — provider[:model[:effort]] for a cold session, or @<job> to continue a
// finished job's conversation (a fan-out reference continues every member and
// must be the only voice). One voice runs as a single turn in the job
// directory itself; several run as a fan-out with a member directory each.
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

// Efforts lists the effort values a provider accepts, so callers can render
// them instead of hardcoding a copy that drifts.
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
	if req.PromptFile == "" {
		return usageError(stderr, "--prompt-file <path> is required")
	}
	if _, err := os.Stat(req.PromptFile); err != nil {
		return usageError(stderr, "prompt file not found: %s", req.PromptFile)
	}
	timeoutMin, err := resolveTimeout(req.TimeoutMin, req.NoTimeout)
	if err != nil {
		return usageError(stderr, "%s", err)
	}

	turns, errText := resolveTurns(req, invocationCwd, timeoutMin)
	if errText != "" {
		return usageError(stderr, "%s", errText)
	}
	if req.MaxBudgetUSD != nil {
		if len(turns) != 1 || turns[0].Options.Provider != "claude" {
			return usageError(stderr, "--max-budget-usd caps one claude voice; codex has no budget flag")
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
	if err := job.Reserve(dir); err != nil {
		if errors.Is(err, job.ErrJobExists) {
			return usageError(stderr, "%s", prose.JobExists(dir))
		}
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
		releaseIfUnstarted(dir, job.Workspace{Dir: dir}.MetaPath())
		return code
	}
	code := fan.Run(fan.Options{Turns: turns, OutDir: dir, Stdout: stdout, Stderr: stderr})
	releaseIfUnstarted(dir, job.GroupWorkspace{Dir: dir}.GroupPath())
	return code
}

// releaseIfUnstarted gives a reserved name back when nothing ran under it: a
// refusal after reservation (a held session, an unreadable prompt) leaves no
// record, and an occupied name with nothing to collect would be invisible.
// Once a record exists the directory is evidence and stays.
func releaseIfUnstarted(dir, recordPath string) {
	if _, err := os.Stat(recordPath); err == nil {
		return
	}
	os.RemoveAll(dir)
}

// voice is one turn of the roster before the shared settings are applied:
// what the caller spelled, resolved into the turn it runs.
type voice struct {
	base       string // the name to derive the member address from
	provider   string
	model      string
	effort     string
	session    string // "" = a fresh conversation
	source     string // the job dir whose conversation session continues
	cwd        string // recorded tree, for a continued voice
	baseline   string // recorded anchor, for a continued voice
	allowWrite bool   // recorded write intent, for a continued voice
}

// resolveTurns turns the voices into the turns a job runs, every refusal
// decided before a directory is reserved or a session locked. A fan-out
// reference expands into its members and each one is resolved exactly as a
// member named directly would be, so there is one eligibility rule and one
// set of roster checks whatever the caller spelled. errText is "" exactly
// when the roster is dispatchable.
func resolveTurns(req RunRequest, invocationCwd string, timeoutMin float64) ([]fan.Turn, string) {
	var voices []voice
	for _, spec := range req.With {
		if !strings.HasPrefix(spec, "@") {
			v, err := parseVoice(spec)
			if err != nil {
				return nil, err.Error()
			}
			voices = append(voices, v)
			continue
		}
		ref, err := job.ResolveRef(strings.TrimPrefix(spec, "@"), invocationCwd)
		if err != nil {
			return nil, fmt.Sprintf("--with %s: %s", spec, err)
		}
		if job.IsGroupDir(ref) {
			if len(req.With) > 1 {
				return nil, prose.GroupRefMustStandAlone(ref, fanCandidates(ref))
			}
			members, errText := fanMembers(ref)
			if errText != "" {
				return nil, errText
			}
			voices = append(voices, members...)
			continue
		}
		source, blocker, err := collect.Inspect(ref)
		if err != nil {
			return nil, prose.ContinueNoTurn(ref, err)
		}
		if blocker != "" {
			return nil, prose.ContinueBlocked(ref, blocker)
		}
		voices = append(voices, continuedVoice(ref, source, ""))
	}

	// The roster checks: one conversation once, and one tree and one anchor
	// across every continued voice unless the caller chose them.
	cwd, baseline := req.Cwd, req.Baseline
	sessions := map[string]string{}
	inheritedCwdFrom, inheritedBaselineFrom := "", ""
	var writeSources []string
	for _, v := range voices {
		if v.session == "" {
			continue
		}
		if prev, dup := sessions[v.session]; dup {
			return nil, prose.DuplicateConversation(v.session, prev, v.source)
		}
		sessions[v.session] = v.source
		if v.allowWrite {
			writeSources = append(writeSources, v.source)
		}
		if req.Cwd == "" && v.cwd != "" {
			if cwd == "" {
				cwd, inheritedCwdFrom = v.cwd, v.source
			} else if v.cwd != cwd {
				return nil, prose.CwdMix(inheritedCwdFrom, cwd, v.source, v.cwd)
			}
		}
		if req.Baseline == "" && v.baseline != "" {
			if baseline == "" {
				baseline, inheritedBaselineFrom = v.baseline, v.source
			} else if v.baseline != baseline {
				return nil, prose.BaselineMix(inheritedBaselineFrom, baseline, v.source, v.baseline)
			}
		}
	}
	if cwd == "" {
		cwd = invocationCwd
	}
	cwd = absOrSelf(cwd)

	// Write intent is checked after expansion: a roster is read-only unless
	// it is one voice, and a continued write conversation keeps its intent
	// only by continuing alone.
	allowWrite := false
	switch {
	case len(voices) == 1:
		allowWrite = req.AllowWrite || len(writeSources) == 1
	case req.AllowWrite:
		return nil, prose.AllowWriteNeedsOneVoice()
	case len(writeSources) > 0:
		return nil, prose.WriteSourceInRoster(writeSources[0], timeoutMin)
	}

	taken := map[string]bool{}
	turns := make([]fan.Turn, len(voices))
	for i, v := range voices {
		turns[i] = fan.Turn{
			Name: fan.Name(v.base, taken),
			Options: runner.Options{
				Provider:    v.provider,
				PromptFile:  req.PromptFile,
				Cwd:         cwd,
				Baseline:    baseline,
				ResumedFrom: v.source,
				TimeoutMin:  timeoutMin,
				Turn: provider.Options{
					Model:        v.model,
					Effort:       v.effort,
					Resume:       v.session,
					AllowWrite:   allowWrite,
					MaxBudgetUSD: req.MaxBudgetUSD,
				},
			},
		}
	}
	return turns, ""
}

// continuedVoice is the voice that continues a finished job's conversation,
// from the source its records describe. name presets the member address (a
// fan-out's member keeps its identity across rounds); "" derives it.
func continuedVoice(dir string, source *collect.Source, name string) voice {
	if name == "" {
		name = voiceBase(source.Provider, source.Model)
	}
	return voice{
		base:       name,
		provider:   source.Provider,
		model:      source.Model,
		effort:     source.Effort,
		session:    source.Session,
		source:     dir,
		cwd:        source.Cwd,
		baseline:   source.Baseline,
		allowWrite: source.AllowWrite,
	}
}

// fanMembers expands a fan-out reference into its members as continued
// voices, each keeping the name it had. The set is whole or refused: any
// member that cannot continue refuses the round rather than being silently
// left out of it.
func fanMembers(dir string) ([]voice, string) {
	gw := job.GroupWorkspace{Dir: dir}
	group, err := job.ReadGroupFile(gw.GroupPath())
	if err != nil {
		return nil, prose.FanContinueUnreadableManifest(dir, err)
	}
	if len(group.Members) == 0 {
		return nil, prose.FanContinueEmptyManifest(dir)
	}
	var voices []voice
	var reasons []string
	for _, name := range group.Members {
		memberDir := gw.Member(name).Dir
		source, blocker, err := collect.Inspect(memberDir)
		switch {
		case err != nil:
			reasons = append(reasons, prose.FanResumeBlockerLine(name, prose.BlockerUnreadableMeta, err.Error()))
			continue
		case blocker != "":
			reasons = append(reasons, prose.FanResumeBlockerLine(name, blocker, ""))
			continue
		}
		voices = append(voices, continuedVoice(memberDir, source, name))
	}
	if len(reasons) > 0 {
		return nil, prose.FanContinueBlocked(dir, reasons)
	}
	return voices, ""
}

// fanCandidates lists a fan-out's members in roster order, each through the
// one eligibility definition, so a refusal never offers a member dispatch
// would refuse.
func fanCandidates(dir string) []prose.FanMemberCandidate {
	gw := job.GroupWorkspace{Dir: dir}
	group, err := job.ReadGroupFile(gw.GroupPath())
	if err != nil {
		return nil
	}
	var candidates []prose.FanMemberCandidate
	for _, name := range group.Members {
		c := prose.FanMemberCandidate{Name: name, Dir: gw.Member(name).Dir}
		_, blocker, err := collect.Inspect(c.Dir)
		switch {
		case err != nil:
			c.Blocked, c.Kind, c.Detail = true, prose.BlockerUnreadableMeta, err.Error()
		case blocker != "":
			c.Blocked, c.Kind = true, blocker
		}
		candidates = append(candidates, c)
	}
	return candidates
}

// parseVoice reads one cold voice. The colon form keeps a voice's settings
// unambiguously attached to it — no provider name, model name or effort value
// contains a colon — where repeated flags could not say which voice they
// belonged to.
func parseVoice(spec string) (voice, error) {
	parts := strings.Split(spec, ":")
	if len(parts) > 3 {
		return voice{}, fmt.Errorf("--with '%s' has too many fields; the form is provider[:model[:effort]], for example claude:opus:high", spec)
	}
	v := voice{provider: parts[0]}
	if len(parts) > 1 {
		v.model = parts[1]
	}
	if len(parts) > 2 {
		v.effort = parts[2]
	}
	if v.provider != "claude" && v.provider != "codex" {
		return voice{}, fmt.Errorf("--with '%s' must name provider claude or codex, got '%s'", spec, v.provider)
	}
	if err := provider.ValidateEffort(v.provider, v.effort); err != nil {
		return voice{}, fmt.Errorf("--with '%s': %s", spec, err)
	}
	v.base = voiceBase(v.provider, v.model)
	return v, nil
}

// voiceBase is the member address a voice derives from what distinguishes
// it: the provider, plus the model when one was named.
func voiceBase(providerName, model string) string {
	if model != "" {
		return providerName + "-" + model
	}
	return providerName
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
	dir, err := job.ResolveRef(req.Job, invocationCwd)
	if err != nil {
		return usageError(stderr, "%s", err)
	}
	return collect.Collect(dir, mode, stdout, stderr)
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

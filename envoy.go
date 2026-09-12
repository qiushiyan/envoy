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
const Version = "0.6.0"

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

// plan is a run request resolved into what dispatch needs: the members, the
// tree and anchor they share, and the write intent — every refusal decided
// before a directory is reserved or a session locked.
type plan struct {
	members    []fan.Member
	cwd        string
	baseline   string
	allowWrite bool
	// groupRef is the fan-out directory every member continues, "" when the
	// roster was composed voice by voice.
	groupRef string
}

// Run validates the request, reserves the job, and runs it to terminal state.
func Run(req RunRequest) int {
	stdout, stderr := defaultWriters(req.Stdout, req.Stderr)

	invocationCwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "envoy: cannot determine cwd: %s\n", err)
		return ExitInfra
	}
	dir, err := job.ResolveRef(req.Job, invocationCwd)
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

	p, errText := resolvePlan(req, invocationCwd, timeoutMin)
	if errText != "" {
		return usageError(stderr, "%s", errText)
	}
	if req.MaxBudgetUSD != nil {
		if len(p.members) != 1 || p.members[0].Provider != "claude" {
			return usageError(stderr, "--max-budget-usd caps one claude voice; codex has no budget flag")
		}
		if math.IsNaN(*req.MaxBudgetUSD) || math.IsInf(*req.MaxBudgetUSD, 0) || *req.MaxBudgetUSD <= 0 {
			return usageError(stderr, "--max-budget-usd must be a positive number")
		}
	}
	cwd := p.cwd
	if cwd == "" {
		cwd = invocationCwd
	}
	cwd = absOrSelf(cwd)
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
	label := filepath.Base(dir)

	if len(p.members) == 1 {
		m := p.members[0]
		code := runner.Run(runner.Options{
			Provider:    m.Provider,
			PromptFile:  req.PromptFile,
			Cwd:         cwd,
			Baseline:    p.baseline,
			Label:       label,
			OutDir:      dir,
			ResumedFrom: m.ResumedFrom,
			Turn: provider.Options{
				Model:        m.Model,
				Effort:       m.Effort,
				Resume:       m.Resume,
				AllowWrite:   p.allowWrite,
				TimeoutMin:   timeoutMin,
				MaxBudgetUSD: req.MaxBudgetUSD,
			},
			Stdout: stdout,
			Stderr: stderr,
		}).ExitCode
		releaseIfUnstarted(dir, job.Workspace{Dir: dir}.MetaPath())
		return code
	}
	code := fan.Run(fan.Options{
		Members:     p.members,
		PromptFile:  req.PromptFile,
		Cwd:         cwd,
		Baseline:    p.baseline,
		Label:       label,
		OutDir:      dir,
		ResumedFrom: p.groupRef,
		TimeoutMin:  timeoutMin,
		Stdout:      stdout,
		Stderr:      stderr,
	})
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

// resolvePlan turns the voices into a roster. Continued voices are resolved
// first, through collect's typed inspection, so the dispatch decision and
// collect's own resume line can never disagree about which jobs may
// continue; every refusal names the job actually named. errText is "" exactly
// when the plan is dispatchable.
func resolvePlan(req RunRequest, invocationCwd string, timeoutMin float64) (plan, string) {
	p := plan{cwd: req.Cwd, baseline: req.Baseline}
	sessionDirs := map[string]string{}
	inheritedCwd, inheritedCwdDir := "", ""
	inheritedBaseline, inheritedBaselineDir := "", ""
	var writeSources []string

	inherit := func(dir, cwd, baseline string) string {
		if req.Cwd == "" && cwd != "" {
			if inheritedCwd == "" {
				inheritedCwd, inheritedCwdDir = cwd, dir
			} else if cwd != inheritedCwd {
				return prose.CwdMix(inheritedCwdDir, inheritedCwd, dir, cwd)
			}
		}
		if req.Baseline == "" && baseline != "" {
			if inheritedBaseline == "" {
				inheritedBaseline, inheritedBaselineDir = baseline, dir
			} else if baseline != inheritedBaseline {
				return prose.BaselineMix(inheritedBaselineDir, inheritedBaseline, dir, baseline)
			}
		}
		return ""
	}

	for _, spec := range req.With {
		if !strings.HasPrefix(spec, "@") {
			m, err := parseMember(spec)
			if err != nil {
				return plan{}, err.Error()
			}
			p.members = append(p.members, m)
			continue
		}
		ref, err := job.ResolveRef(strings.TrimPrefix(spec, "@"), invocationCwd)
		if err != nil {
			return plan{}, fmt.Sprintf("--with %s: %s", spec, err)
		}
		if job.IsGroupDir(ref) {
			if len(req.With) > 1 {
				return plan{}, prose.GroupRefMustStandAlone(ref, fanCandidates(ref))
			}
			members, group, errText := resumableMembers(ref)
			if errText != "" {
				return plan{}, errText
			}
			if errText := inherit(ref, group.Cwd, derefString(group.GitBaseline)); errText != "" {
				return plan{}, errText
			}
			p.members = members
			p.groupRef = ref
			break
		}
		source, errText := resumableTurn(ref)
		if errText != "" {
			return plan{}, errText
		}
		if prev, dup := sessionDirs[source.Session]; dup {
			return plan{}, prose.DuplicateConversation(source.Session, prev, ref)
		}
		sessionDirs[source.Session] = ref
		if source.AllowWrite {
			writeSources = append(writeSources, ref)
		}
		if errText := inherit(ref, source.Cwd, source.Baseline); errText != "" {
			return plan{}, errText
		}
		p.members = append(p.members, fan.Member{
			Provider:    source.Provider,
			Model:       source.Model,
			Effort:      source.Effort,
			Resume:      source.Session,
			ResumedFrom: ref,
		})
	}
	if p.cwd == "" {
		p.cwd = inheritedCwd
	}
	if p.baseline == "" {
		p.baseline = inheritedBaseline
	}

	// Write intent is checked after expansion: a roster is read-only unless
	// it is one voice, and a continued write conversation keeps its intent
	// only by continuing alone.
	if len(p.members) == 1 {
		p.allowWrite = req.AllowWrite || len(writeSources) == 1
		return p, ""
	}
	if req.AllowWrite {
		return plan{}, prose.AllowWriteNeedsOneVoice()
	}
	if len(writeSources) > 0 {
		return plan{}, prose.WriteSourceInRoster(writeSources[0], timeoutMin)
	}
	return p, ""
}

// resumableTurn resolves a job dir named by @<job> into the turn it may
// continue. errText is "" exactly when the turn is continuable.
func resumableTurn(dir string) (*collect.ResumableTurn, string) {
	source, blocker, err := collect.InspectTurnResume(dir)
	if err != nil {
		return nil, prose.ContinueNoTurn(dir, err)
	}
	if blocker != "" {
		return nil, prose.ContinueBlocked(dir, blocker)
	}
	return source, ""
}

// fanCandidates lists a fan-out's members in roster order, each through the
// one eligibility definition, so a refusal never offers a member dispatch
// would refuse.
func fanCandidates(dir string) []prose.FanMemberCandidate {
	state, err := collect.InspectFanResume(dir)
	if err != nil {
		return nil
	}
	var candidates []prose.FanMemberCandidate
	for _, m := range state.Group.Members {
		c := prose.FanMemberCandidate{Name: m.Name, Dir: m.OutDir}
		for _, b := range state.Blockers {
			if b.Member == m.Name {
				c.Blocked, c.Kind, c.Detail = true, b.Kind, b.Detail
			}
		}
		candidates = append(candidates, c)
	}
	return candidates
}

// resumableMembers resolves a fan-out directory into the members of a new
// round, through collect's typed inspection. The set is whole or refused: any
// blocked member refuses the round rather than being silently left out of it.
func resumableMembers(dir string) ([]fan.Member, *job.Group, string) {
	state, err := collect.InspectFanResume(dir)
	if err != nil {
		return nil, nil, prose.FanContinueUnreadableManifest(dir, err)
	}
	if len(state.Members) == 0 && len(state.Blockers) == 0 {
		return nil, nil, prose.FanContinueEmptyManifest(dir)
	}
	if len(state.Blockers) > 0 {
		reasons := make([]string, len(state.Blockers))
		for i, b := range state.Blockers {
			reasons[i] = prose.FanResumeBlockerLine(b.Member, b.Kind, b.Detail)
		}
		return nil, nil, prose.FanContinueBlocked(dir, reasons)
	}
	members := make([]fan.Member, len(state.Members))
	for i, m := range state.Members {
		members[i] = fan.Member{
			Provider:    m.Provider,
			Model:       m.Model,
			Effort:      m.Effort,
			Resume:      m.Session,
			ResumedFrom: m.OutDir,
			Name:        m.Name,
		}
	}
	return members, state.Group, ""
}

// parseMember reads one cold voice. The colon form keeps a voice's settings
// unambiguously attached to it — no provider name, model name or effort value
// contains a colon — where repeated flags could not say which voice they
// belonged to.
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
	Job        string // the job's name in this project's store, or its directory
	ResultOnly bool   // an ok job's result body alone; a non-ok job prints its full block
	StatusOnly bool   // everything except the result body; marks nothing collected
	Stdout     io.Writer
	Stderr     io.Writer
}

// Collect prints one job and stamps first terminal collection — except under
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

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

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
const Version = "0.10.0"

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
		releaseIfUnstarted(dir, job.Workspace{Dir: dir}.MetaPath())
		return code
	}
	code := fan.Run(fan.Options{Turns: turns, OutDir: dir, Stdout: stdout, Stderr: stderr})
	releaseIfUnstarted(dir, job.GroupWorkspace{Dir: dir}.GroupPath())
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
	promptFile string // the prompt this voice alone was given; "" = the job's default
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
func resolveTurns(req RunRequest, invocationCwd, caller string, timeoutMin float64) ([]fan.Turn, string) {
	var voices []voice
	for _, seat := range req.With {
		spec, promptFile, errText := splitSeat(seat)
		if errText != "" {
			return nil, errText
		}
		if promptFile == "" {
			promptFile = req.PromptFile
		}
		if promptFile == "" {
			return nil, prose.VoiceNeedsPrompt(spec)
		}
		if !strings.HasPrefix(spec, "@") {
			v, err := parseVoice(spec)
			if err != nil {
				return nil, err.Error()
			}
			v.promptFile = promptFile
			voices = append(voices, v)
			continue
		}
		ref, err := job.ResolveRef(strings.TrimPrefix(spec, "@"), invocationCwd, caller)
		if err != nil {
			return nil, fmt.Sprintf("--with %s: %s", spec, err)
		}
		if job.IsGroupDir(ref) {
			if len(req.With) > 1 {
				return nil, prose.GroupRefMustStandAlone(ref, fanCandidates(ref))
			}
			members, errText := fanMembers(ref, promptFile)
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
		v := continuedVoice(ref, source, "")
		v.promptFile = promptFile
		voices = append(voices, v)
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
				PromptFile:  v.promptFile,
				Cwd:         cwd,
				Baseline:    baseline,
				ResumedFrom: v.source,
				Caller:      caller,
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

// splitSeat separates what the caller spelled into the voice and, when one
// was attached with '=', that voice's own prompt file. The first '=' splits:
// no provider, model, effort or job name contains one, and the prompt path
// to its right may. A job named by a directory path is the one form that
// could carry '=' legitimately, and there it is refused rather than guessed
// at — but only when such a directory actually exists, since otherwise the
// spelling can only have meant an attachment.
func splitSeat(seat string) (spec, promptFile, errText string) {
	i := strings.IndexByte(seat, '=')
	if i < 0 {
		return seat, "", ""
	}
	if strings.HasPrefix(seat, "@") {
		if whole := strings.TrimPrefix(seat, "@"); job.IsPath(whole) {
			if info, err := os.Stat(whole); err == nil && info.IsDir() {
				return "", "", prose.JobPathHasEquals(seat)
			}
		}
	}
	spec, promptFile = seat[:i], seat[i+1:]
	if promptFile == "" {
		return "", "", fmt.Sprintf("--with %s: give the prompt file after '=' (--with %s<file>), or drop the '=' to use --prompt-file", seat, seat)
	}
	return spec, promptFile, ""
}

// fanMembers expands a fan-out reference into its members as continued
// voices, each keeping the name it had and each sent promptFile — the round's
// one NEW prompt. The set is whole or refused: any member that cannot continue
// refuses the round rather than being silently left out of it.
func fanMembers(dir, promptFile string) ([]voice, string) {
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
		v := continuedVoice(memberDir, source, name)
		v.promptFile = promptFile
		voices = append(voices, v)
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
	dir, err := job.ResolveRef(req.Job, invocationCwd, job.CallerFromEnv())
	if msg, ok := unresolvable(err); ok {
		fmt.Fprintf(stderr, "collect error: %s\n", msg)
		return ExitInfra
	}
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

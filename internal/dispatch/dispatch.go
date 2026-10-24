// Package dispatch runs one job: it checks what the caller asked for,
// resolves the roster of voices into the turns they run, reserves the job's
// directory, and runs one turn flat in it or several as a fan-out. Every
// refusal is decided before a directory is reserved or a session locked.
//
// The command line is the only caller; it runs a dispatch in a process of its
// own, so the turn outlives the command that started it.
package dispatch

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/qiushiyan/envoy/internal/collect"
	"github.com/qiushiyan/envoy/internal/fan"
	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
	"github.com/qiushiyan/envoy/internal/provider"
	"github.com/qiushiyan/envoy/internal/runner"
)

// Request describes one job: a caller-chosen name (or directory), a
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
// voice inherits its provider, session, model, effort, cwd, baseline, write
// intent and standing instructions from the job's own records; explicit Cwd
// and Baseline win, and AllowWrite may only widen a single continued voice,
// never narrow it.
//
// InstructionsFile holds the standing instructions of every conversation the
// job starts: system-level text the provider receives beside each turn's
// prompt, archived in the job directory and passed again on every turn that
// continues the conversation. A conversation keeps the instructions it
// started with, so on a continued voice the file must match them.
type Request struct {
	Job              string
	With             []string
	PromptFile       string
	InstructionsFile string
	Baseline         string
	AllowWrite       bool
	Cwd              string
	TimeoutMin       float64 // hard wall-clock cap in minutes; 0 = the 30-minute default
	NoTimeout        bool    // explicitly disable the cap (leave TimeoutMin zero)
	// MaxBudgetUSD caps one claude voice's spend — the safety a program
	// dispatching unattended turns needs; nil = no cap.
	MaxBudgetUSD *float64

	Stdout io.Writer // dispatch and terminal blocks
	Stderr io.Writer // errors and warnings
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

// Run validates the request, reserves the job, and runs it to terminal state.
// code is the process exit code. A refusal made before the job could run —
// the caller's mistake (3) or the environment's (2) — comes back as err,
// unprinted, for the command line to report; what the job's own run reports,
// it has already printed, and err is nil.
func Run(req Request) (code int, err error) {
	invocationCwd, err := os.Getwd()
	if err != nil {
		return job.ExitInfra, fmt.Errorf("cannot determine cwd: %s", err)
	}
	dir, err := job.ResolveName(req.Job, invocationCwd)
	if err != nil {
		return job.ExitUsage, err
	}
	// The command line requires a voice before it calls; the check keeps a
	// request without one from reaching the roster, which indexes its first.
	if len(req.With) == 0 {
		return job.ExitUsage, errors.New(prose.RunNeedsVoice())
	}
	// A default the caller named must read even if every voice overrides it:
	// a mistyped path is a mistake whatever the roster does with it.
	if req.PromptFile != "" {
		if err := promptReadable(req.PromptFile); err != nil {
			return job.ExitUsage, fmt.Errorf("prompt file %s", err)
		}
	}
	timeoutMin, err := resolveTimeout(req.TimeoutMin, req.NoTimeout)
	if err != nil {
		return job.ExitUsage, err
	}

	instructions := ""
	if req.InstructionsFile != "" {
		if instructions, err = readInstructions(req.InstructionsFile); err != nil {
			return job.ExitUsage, fmt.Errorf("instructions file %s", err)
		}
	}

	caller := job.CallerFromEnv()
	turns, err := resolveTurns(req, instructions, invocationCwd, caller, timeoutMin)
	if msg, storeFailed := collect.Unresolvable(err); storeFailed {
		return job.ExitInfra, errors.New(msg)
	}
	if err != nil {
		return job.ExitUsage, err
	}
	// Every turn's prompt is checked before the name is reserved, so a
	// member's unreadable file refuses the whole dispatch rather than
	// starting its siblings.
	for _, t := range turns {
		if err := promptReadable(t.Options.PromptFile); err != nil {
			return job.ExitUsage, fmt.Errorf("prompt file %s", err)
		}
	}
	if req.MaxBudgetUSD != nil {
		if len(turns) != 1 || !provider.SpendCap(turns[0].Options.Provider) {
			return job.ExitUsage, errors.New(spendCapRefusal())
		}
		if math.IsNaN(*req.MaxBudgetUSD) || math.IsInf(*req.MaxBudgetUSD, 0) || *req.MaxBudgetUSD <= 0 {
			return job.ExitUsage, errors.New("--max-budget-usd must be a positive number")
		}
	}
	cwd := turns[0].Options.Cwd
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return job.ExitUsage, fmt.Errorf("cwd not found: %s", cwd)
	}

	// The name is reserved last, after every refusal that needs no
	// reservation, and released again on any refusal that happens before the
	// job wrote its first record.
	dir, refusal, err := reserve(req.Job, dir, caller)
	if refusal != "" {
		return job.ExitUsage, errors.New(refusal)
	}
	if msg, ok := collect.Unresolvable(err); ok {
		return job.ExitInfra, errors.New(msg)
	}
	if err != nil {
		return job.ExitInfra, fmt.Errorf("cannot create job dir: %s", err)
	}

	// Cardinality decides layout only: one voice runs flat in the job
	// directory, several run as a fan-out with a member directory each.
	if len(turns) == 1 {
		opts := turns[0].Options
		opts.OutDir = dir
		opts.Stdout, opts.Stderr = req.Stdout, req.Stderr
		code := runner.Run(opts).ExitCode
		releaseIfUnstarted(dir)
		return code, nil
	}
	code = fan.Run(fan.Options{Turns: turns, OutDir: dir, Stdout: req.Stdout, Stderr: req.Stderr})
	releaseIfUnstarted(dir)
	return code, nil
}

// reserve claims the directory this dispatch runs in. A path is an identity
// and is taken or refused as given. A name gets a generation of its own,
// always the next one, and from then on means that job to this caller.
//
// What can still refuse it is a job this dispatch would take the name away
// from while a caller may be waiting to collect by it: the job the name means
// to this caller now, and the newest one, which is what the name means to
// every caller without a generation of its own — each refusing only when it
// holds the name against this caller (collect.NameHold). Losing the Mkdir to
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
				if hold, held := collect.NameHold(holder, caller); held {
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

// readInstructions reads the standing instructions once, before anything is
// reserved; that one read is what every turn of the job is sent and archives.
// They are text a conversation keeps for its lifetime, so an empty file — a
// conversation with none is one dispatched without the flag — or one that is
// not UTF-8 is a mistake to refuse, not a text to carry on.
func readInstructions(path string) (string, error) {
	if err := promptReadable(path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	switch {
	case err != nil:
		return "", fmt.Errorf("cannot be read: %s", err)
	case strings.TrimSpace(string(data)) == "":
		return "", fmt.Errorf("is empty: %s; a conversation without standing instructions is dispatched without --instructions-file", path)
	case !utf8.Valid(data):
		return "", fmt.Errorf("is not UTF-8 text: %s", path)
	}
	return string(data), nil
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

func absOrSelf(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

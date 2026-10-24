// Command envoy runs headless AI-session turns as named jobs and returns
// them as data. The grammar is declared once, as the Kong structs below, and
// clikit renders every help page from it; the prose an agent reads lives in
// help.go.
package main

import (
	"os"
	"slices"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit"

	"github.com/qiushiyan/envoy/internal/collect"
	"github.com/qiushiyan/envoy/internal/detach"
	"github.com/qiushiyan/envoy/internal/dispatch"
	"github.com/qiushiyan/envoy/internal/job"
)

// version is the engine's, printed by `envoy version`. It changes with every
// change to what callers read.
const version = "0.15.0"

type CLI struct {
	VersionFlag kong.VersionFlag `name:"version" help:"Print the version and exit."`

	Run     RunCmd     `cmd:"" help:"Dispatch a job — one turn, or a fan-out of several — and wait until it ends."`
	Collect CollectCmd `cmd:"" help:"Print a job's result, or how it ended, and the one action to take next."`
	Wait    WaitCmd    `cmd:"" help:"Block until a running job ends, then say how it ended."`
	Pending PendingCmd `cmd:"" help:"List the jobs still needing attention after a missed completion."`
	Version VersionCmd `cmd:"" help:"Print the version."`

	// The two processes a dispatch starts to detach its turn. Their names are
	// detach.SpawnEntry and detach.DetachedEntry, and an installed binary can
	// be replaced between a waiter starting and its exec, so they never change.
	Spawn    SpawnCmd    `cmd:"" name:"__envoy-spawn" hidden:"" passthrough:"" help:"Start the detached process and exit."`
	Detached DetachedCmd `cmd:"" name:"__envoy-detached" hidden:"" help:"Run a dispatch detached from its caller."`
}

type RunCmd struct {
	Job              string   `arg:"" help:"A name for the job, which you collect it by: one path segment of letters, digits, '.', '_' and '-', kept in this project's store. Or a directory path."`
	With             []string `required:"" sep:"none" placeholder:"VOICE[=FILE]" help:"A voice: provider[:model[:effort]] for a cold session, or @<job> to continue a finished job's conversation; =FILE gives that voice a prompt of its own. Once per voice, in the order they are listed. See Voices."`
	PromptFile       string   `placeholder:"FILE" help:"The whole prompt, for every voice without a FILE of its own; required unless each voice carries one. Copied into the job directory."`
	InstructionsFile string   `placeholder:"FILE" help:"Standing instructions for each conversation the job starts, sent as system-level text beside every turn's prompt. See Standing instructions."`
	TimeoutMin       float64  `default:"30" placeholder:"MINUTES" help:"Wall-clock safety cap; 0 runs without one. Each member of a fan-out gets it separately."`
	Cwd              string   `placeholder:"DIR" help:"The directory the session works in, instead of the current one (or a continued job's own)."`
	Baseline         string   `placeholder:"SHA" help:"The commit collect diffs the working tree against; a write turn defaults to HEAD."`
	AllowWrite       bool     `help:"Record that a single voice will edit files, so collect diffs its work. It restricts nothing; see What the flags cannot tell you."`
	MaxBudgetUSD     *float64 `name:"max-budget-usd" placeholder:"USD" help:"Spend cap for a single claude voice, the safety net of a program dispatching unattended turns; refused for a codex voice or a fan-out, which have no budget flag."`
}

// Run is the waiter: the dispatch runs in a process of its own, which parses
// the same words with this same declaration, and this one relays its output
// and exits with its code.
func (c *RunCmd) Run(ctx *kong.Context) error {
	tail := ctx.Args[slices.Index(ctx.Args, "run")+1:]
	return clikit.Exit(detach.Dispatch(c.Job, tail, ctx.Stdout, ctx.Stderr))
}

// request is the dispatch the flags describe. The command line keeps
// --timeout-min 0 for no cap; the engine spells that NoTimeout so the
// uncapped state is never a zero value.
func (c *RunCmd) request(ctx *kong.Context) dispatch.Request {
	req := dispatch.Request{
		Job:              c.Job,
		With:             c.With,
		PromptFile:       c.PromptFile,
		InstructionsFile: c.InstructionsFile,
		Baseline:         c.Baseline,
		AllowWrite:       c.AllowWrite,
		Cwd:              c.Cwd,
		MaxBudgetUSD:     c.MaxBudgetUSD,
		Stdout:           ctx.Stdout,
		Stderr:           ctx.Stderr,
	}
	if c.TimeoutMin == 0 {
		req.NoTimeout = true
	} else {
		req.TimeoutMin = c.TimeoutMin
	}
	return req
}

type DetachedCmd struct {
	RunCmd `embed:""`
}

func (c *DetachedCmd) Run(ctx *kong.Context) error {
	return outcome(dispatch.Run(c.request(ctx)))
}

type SpawnCmd struct {
	Args []string `arg:"" optional:"" help:"The words of the run being detached."`
}

func (c *SpawnCmd) Run() error {
	return clikit.Exit(detach.Spawn(c.Args))
}

type CollectCmd struct {
	Job        string `arg:"" help:"The job: the name it was run as, or its directory."`
	ResultOnly bool   `xor:"mode" help:"Print an ok job's result alone. A job that is not ok prints its whole block, since its status and next action are its result."`
	StatusOnly bool   `xor:"mode" help:"Print everything but the result, including the diagnostics an ok job's block holds back. Marks nothing collected."`
}

func (c *CollectCmd) Run(ctx *kong.Context) error {
	mode := collect.ModeFull
	switch {
	case c.ResultOnly:
		mode = collect.ModeResultOnly
	case c.StatusOnly:
		mode = collect.ModeStatusOnly
	}
	return outcome(collect.Collect(c.Job, mode, ctx.Stdout, ctx.Stderr))
}

type WaitCmd struct {
	Job string `arg:"" help:"The job to wait for: the name it was run as, or its directory."`
}

func (c *WaitCmd) Run(ctx *kong.Context) error {
	return outcome(collect.Wait(c.Job, ctx.Stdout, ctx.Stderr))
}

type PendingCmd struct {
	Base string `placeholder:"DIR" help:"The job store to search, instead of the current project's."`
}

func (c *PendingCmd) Run(ctx *kong.Context) error {
	return outcome(collect.Pending(c.Base, ctx.Stdout, ctx.Stderr))
}

type VersionCmd struct{}

func (VersionCmd) Run(ctx *kong.Context) error {
	_, err := ctx.Stdout.Write([]byte("envoy " + version + "\n"))
	return err
}

// outcome ends a command with the engine's exit code. A refusal the engine
// made before it reported anything is printed here, once, as the command's
// error; everything else the engine has already printed.
func outcome(code int, err error) error {
	if err != nil {
		return clikit.Fail(code, err)
	}
	return clikit.Exit(code)
}

var app = clikit.App{
	Name:        "envoy",
	Description: "envoy runs headless claude or codex turns as jobs you name, and returns each as files.",
	New:         func() any { return &CLI{} },
	Options:     []kong.Option{kong.Vars{"version": "envoy " + version}},
	UsageCode:   job.ExitUsage,
	FailureCode: job.ExitInfra,
	ExitCodes: map[int]string{
		job.ExitFailed:      "failed: the provider ran and reported a failure; partial work may exist",
		job.ExitInfra:       "infra: envoy or the environment failed, not the model",
		job.ExitUsage:       "refused: the command, a held name or a locked session",
		job.ExitTimeout:     "timeout: the cap elapsed, which is not evidence the provider hung",
		job.ExitInterrupted: "interrupted: Ctrl-C on the dispatch, or its stop: command",
		job.ExitPartial:     "partial, a fan-out only: some members returned a result and others did not",
	},
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is main without the exit. The detached process reports its PID before
// the dispatch and its exit code after, around the whole run, so even a run
// that fails to parse tells its waiter how it ended.
func run(args []string) int {
	if len(args) > 0 && args[0] == detach.DetachedEntry {
		return detach.Serve(func() int { return app.Run(args) })
	}
	return app.Run(args)
}

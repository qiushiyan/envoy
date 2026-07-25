// Command envoy runs one headless AI-session turn and returns it as data.
//
//	envoy turn --provider <claude|codex> --prompt-file <F> [flags]
//	envoy collect [out-dir]
//	envoy pending [--base DIR]
//	envoy version
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"github.com/qiushiyan/envoy"
)

// usageText is read by the agent driving this CLI, so it teaches the workflow
// and what each exit code licenses — not just the flag list. Effort values are
// rendered from the provider package so help and validation cannot disagree.
var usageText = fmt.Sprintf(`envoy — run one headless AI-session turn (claude or codex) and return it as data

  envoy turn --provider <claude|codex> --prompt-file <F> [flags]
  envoy collect [job-dir]        print one job: status, coordinates, result.md
  envoy pending [--base DIR]     jobs still needing attention, after a missed completion
  envoy version

Dispatch a turn, let the command run to completion, then collect it. A turn
can take minutes to hours, so run it as a background job and collect when the
process exits. Tailing the logs shows progress, never completion: a quiet log
means the model is thinking.

Every turn writes a job directory: prompt.md (what was sent), result.md (the
return value), meta.json (status and recovery coordinates), progress.log,
raw.log, stderr.log. collect reads them for you; the newest job for the
current project is the default.

turn flags:
  --provider        claude or codex (required)
  --prompt-file     the full prompt (required); it is copied into the job dir
  --model           model override; omitted means the provider's own config chooses
  --effort          claude: %s · codex: %s
  --resume ID       continue an existing session with a new prompt; never across providers
  --allow-write     let the turn edit files and run commands unattended
  --baseline SHA    diff anchor for collect; write turns default to HEAD
  --cwd DIR         directory the provider works in (default: current dir)
  --out-dir DIR     job directory (default: the central store, ~/.local/state/envoy/jobs/<project>/)
  --timeout-min N   wall-clock safety cap in minutes, 0 = off (default 30)
  --max-budget-usd  per-turn cost cap (claude only)
  --label TEXT      names the job dir (default: provider name)

exit codes, and what each one licenses:
  0 ok           result.md holds the turn's answer
  1 failed       the provider ran and reported a failure; partial work may exist
  2 infra        envoy or the environment failed, not the model
  3 usage        flags were rejected, or the session is locked; nothing ran
  4 timeout      the cap elapsed — not evidence the provider hung
  5 interrupted  a signal stopped the turn
  For 1, 2, 4, and 5, collect the job and follow its recovery line rather than
  re-sending the prompt: a turn the provider accepted may already have changed
  the working tree.
`,
	strings.Join(envoy.Efforts("claude"), " "),
	strings.Join(envoy.Efforts("codex"), " "))

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return envoy.ExitUsage
	}
	switch args[0] {
	case "turn":
		return cmdTurn(args[1:], stdout, stderr)
	case "collect":
		return cmdCollect(args[1:], stdout, stderr)
	case "pending":
		return cmdPending(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "envoy %s\n", envoy.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	default:
		fmt.Fprintf(stderr, "usage error: unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usageText)
		return envoy.ExitUsage
	}
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }
	return fs
}

// parseFlags runs fs over args. An explicit help request prints usage to
// stdout and reports success — an agent reading exit codes must not see a
// "usage error" for asking for help.
func parseFlags(fs *flag.FlagSet, args []string, stdout io.Writer) (proceed bool, code int) {
	err := fs.Parse(args)
	if err == nil {
		return true, 0
	}
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, usageText)
		return false, 0
	}
	return false, envoy.ExitUsage
}

func cmdTurn(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("turn", stderr)
	var req envoy.TurnRequest
	fs.StringVar(&req.Provider, "provider", "", "")
	fs.StringVar(&req.PromptFile, "prompt-file", "", "")
	fs.StringVar(&req.Model, "model", "", "")
	fs.StringVar(&req.Effort, "effort", "", "")
	fs.StringVar(&req.Resume, "resume", "", "")
	fs.StringVar(&req.Baseline, "baseline", "", "")
	fs.BoolVar(&req.AllowWrite, "allow-write", false, "")
	fs.StringVar(&req.Cwd, "cwd", "", "")
	fs.StringVar(&req.OutDir, "out-dir", "", "")
	timeoutMin := fs.Float64("timeout-min", 30, "")
	budget := fs.Float64("max-budget-usd", math.NaN(), "")
	fs.StringVar(&req.Label, "label", "", "")
	if proceed, code := parseFlags(fs, args, stdout); !proceed {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage error: unexpected argument %q\n", fs.Arg(0))
		return envoy.ExitUsage
	}
	// The CLI contract keeps `--timeout-min 0` = no cap; the library spells
	// that NoTimeout so the dangerous state is never a zero value.
	if *timeoutMin == 0 {
		req.NoTimeout = true
	} else {
		req.TimeoutMin = *timeoutMin
	}
	if !math.IsNaN(*budget) {
		req.MaxBudgetUSD = budget
	}
	req.Stdout = stdout
	req.Stderr = stderr
	return envoy.Turn(req)
}

func cmdCollect(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("collect", stderr)
	if proceed, code := parseFlags(fs, args, stdout); !proceed {
		return code
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "collect error: pass at most one out-dir")
		return envoy.ExitUsage
	}
	return envoy.Collect(fs.Arg(0), stdout, stderr)
}

func cmdPending(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("pending", stderr)
	base := fs.String("base", "", "")
	if proceed, code := parseFlags(fs, args, stdout); !proceed {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "collect error: pending does not take an out-dir; use --base to choose the job root")
		return envoy.ExitUsage
	}
	return envoy.Pending(*base, stdout, stderr)
}

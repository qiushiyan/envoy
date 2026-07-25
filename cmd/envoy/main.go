// Command envoy runs one headless AI-session turn and returns it as data.
//
//	envoy turn --provider <claude|codex> --prompt-file <F> [flags]
//	envoy collect [out-dir]
//	envoy pending [--base DIR]
//	envoy version
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/qiushiyan/envoy"
)

const usageText = `envoy — run one headless AI-session turn (claude or codex) and return it as data

usage:
  envoy turn --provider <claude|codex> --prompt-file <F> [flags]
  envoy collect [out-dir]
  envoy pending [--base DIR]
  envoy version

turn flags:
  --provider        claude or codex (required)
  --prompt-file     full prompt file, copied to <out-dir>/prompt.md (required)
  --model           provider model override; omitted = the provider's own config
  --effort          claude: low medium high xhigh max · codex: none minimal low medium high xhigh max ultra
  --resume ID       continue the same provider session (never cross providers)
  --allow-write     claude: bypassPermissions; codex: ~/.codex/config.toml governs
  --baseline SHA    review anchor; write turns default to HEAD
  --cwd DIR         provider working directory (default: current dir)
  --out-dir DIR     job directory (default: <repo>/.envoy/<stamp>-<label>)
  --timeout-min N   hard wall-clock cap, 0 = off (default 30)
  --max-budget-usd  claude-only per-turn cost cap
  --label TEXT      job-dir label (default: provider name)

exit codes: 0 ok · 1 provider failure · 2 infra · 3 usage · 4 timeout · 5 interrupted
`

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
	fs.Float64Var(&req.TimeoutMin, "timeout-min", 30, "")
	budget := fs.Float64("max-budget-usd", math.NaN(), "")
	fs.StringVar(&req.Label, "label", "", "")
	if err := fs.Parse(args); err != nil {
		return envoy.ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage error: unexpected argument %q\n", fs.Arg(0))
		return envoy.ExitUsage
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
	if err := fs.Parse(args); err != nil {
		return envoy.ExitUsage
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
	if err := fs.Parse(args); err != nil {
		return envoy.ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "collect error: pending does not take an out-dir; use --base to choose the job root")
		return envoy.ExitUsage
	}
	return envoy.Pending(*base, stdout, stderr)
}

// Command envoy runs headless AI-session turns as named jobs and returns
// them as data.
//
//	envoy run <job> --prompt-file <F> --with <voice> [--with <voice>…] [flags]
//	envoy collect [--result-only|--status-only] <job>
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

// usageText is the tool description an agent reads before driving envoy. It
// carries the loop, the voice grammar, and the facts that decide whether a
// retry is safe — and nothing a caller does not act on. Effort values render
// from the provider package so the help and the validation cannot disagree.
var usageText = fmt.Sprintf(`envoy — run headless AI-session turns (claude or codex) as named jobs

USAGE
  envoy run <job> --prompt-file <F> --with <voice> [--with <voice>…] [flags]
  envoy collect [--result-only|--status-only] <job>
  envoy pending [--base DIR]      jobs still needing attention after a missed completion
  envoy version

THE LOOP
  1. Write the whole prompt to a file. The session starts cold — it has none
     of your conversation — so that file carries every fact it needs.
  2. Name the job and dispatch. Run the command in the background and let it
     run to completion (minutes to hours); the process exiting is the
     completion signal. You chose the name, so there is nothing to read back.
  3. Collect by that name. It prints the result and, when something went
     wrong, the one action to take next.

    envoy run review-r1 --prompt-file brief.md --with codex --timeout-min 30
    envoy collect review-r1

  A <job> is a name (one path segment; letters, digits, . _ -) kept in this
  project's store under ~/.local/state/envoy, or a directory path. A name is
  used once: running it again is refused, so a round 2 gets a new name.

VOICES
  --with provider[:model[:effort]]   a cold session
    --with codex                     the provider's own configured model
    --with claude:opus               a named model
    --with codex:gpt-6-astra:high    model and effort
    --with codex::high               effort alone; the model slot stays empty
  --with @<job>                      continue that finished job's conversation
    --with @consult-r1                        round 2 in the same session
    --with @consult-r1/codex --with claude:opus
                                     one member warm beside a cold voice
    --with @consult-r1  (alone)      continue every member of a fan-out

  One --with is a single turn; several are a fan-out — the same prompt to each,
  supervised as one job that exits once every member is done, and collected
  once. Members never see each other's output. A fan-out is read-only: its
  members share one working tree.

  A continued voice takes its provider, session, model, effort, tree, baseline
  and write intent from the job's own records; --cwd and --baseline override.
  Provider and model cannot change across a continuation. A fan-out reference
  stands alone: to seat one of its members beside others, name the member.

FLAGS
  --prompt-file F   the full prompt (required); copied into the job dir
  --with VOICE      once per voice (required); see VOICES
  --timeout-min N   wall-clock safety cap in minutes, 0 = off (default 30);
                    each member of a fan-out gets it separately
  --cwd DIR         directory the session works in (default: current dir)
  --baseline SHA    diff anchor collect prints; write turns default to HEAD
  --allow-write     let a single voice edit files and run commands unattended
  --max-budget-usd  spend cap for a single claude voice (a program's safety net)

WHAT A JOB LEAVES BEHIND
  result.md     the answer — this is the return value
  prompt.md     exactly what was sent
  meta.json     status, session id, and recovery coordinates
  progress.log  lifecycle milestones and a heartbeat
  raw.log       the provider's own event stream, verbatim
  stderr.log    the provider's stderr
  A fan-out holds group.json and one such directory per member, named after
  the member (codex, claude-opus, codex-2 …).

FACTS THE FLAGS CANNOT TELL YOU
  Model and effort. Leaving them off puts the provider's own configuration in
  charge; envoy never substitutes a model of its own and reports
  `+"`(provider default)`"+` rather than guessing. The model the provider says it ran is
  recorded either way; `+"`collect --status-only`"+` prints it, and a difference from the
  request is the provider's own aliasing, not a substitution.

  Write intent, not a sandbox. Without --allow-write the session is
  effectively read-only, and "analyse only, change nothing" still belongs in
  the prompt — envoy does not enforce it.

  Continuing. A finished job's block prints `+"`resume:`"+`, the exact command that
  continues its conversation as a new job; it takes a NEW prompt file, since
  re-sending the original repeats work the provider already did. One live
  turn per session: continuing a job that is still running is refused.

  The cap bounds wall-clock time as a safety net, not a stall detector — it
  counts healthy work, so reaching it never proves a hang.

EXIT CODES, AND WHAT EACH ONE LICENSES
  0 ok           result.md holds the answer
  1 failed       the provider ran and reported a failure; partial work may exist
  2 infra        envoy or the environment failed, not the model
  3 usage        flags were rejected, the name is taken, or the session is
                 locked; nothing ran
  4 timeout      the cap elapsed — not evidence the provider hung
  5 interrupted  a signal stopped the turn
  6 partial      fan-out only: some members returned a result and others did
                 not — the results that landed are usable, and only the members
                 that failed need a decision

  For any non-zero exit, collect the job and follow its `+"`next:`"+` line instead of
  re-dispatching: whether the provider accepted the prompt decides between a
  safe retry and duplicating work that already changed the tree, and the job
  knows which happened. After a crash or restart, `+"`envoy pending`"+` finds the jobs
  whose completion you may have missed.

  Efforts — claude: %s · codex: %s
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
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "collect":
		return cmdCollect(args[1:], stdout, stderr)
	case "pending":
		return cmdPending(args[1:], stdout, stderr)
	case "version", "--version", "-v":
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

// stringList collects a repeatable flag in the order it was given, which is
// the order voices are dispatched and listed.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, " ") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// splitJobArg takes the one positional argument — the job — from wherever
// it sits among the flags, so `envoy run review-r1 --with codex` and
// `envoy run --with codex review-r1` both parse.
func splitJobArg(args []string, boolFlags ...string) (job string, rest []string, extra []string) {
	rest = make([]string, 0, len(args))
	isBool := func(a string) bool {
		name := strings.TrimLeft(a, "-")
		for _, b := range boolFlags {
			if name == b {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			// A flag with a separate value carries the value along; a
			// --flag=value spelling and a boolean flag take none.
			if !strings.Contains(a, "=") && !isBool(a) && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if job == "" {
			job = a
		} else {
			extra = append(extra, a)
		}
	}
	return job, rest, extra
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("run", stderr)
	var req envoy.RunRequest
	var with stringList
	fs.Var(&with, "with", "")
	fs.StringVar(&req.PromptFile, "prompt-file", "", "")
	fs.StringVar(&req.Baseline, "baseline", "", "")
	fs.BoolVar(&req.AllowWrite, "allow-write", false, "")
	fs.StringVar(&req.Cwd, "cwd", "", "")
	timeoutMin := fs.Float64("timeout-min", 30, "")
	budget := fs.Float64("max-budget-usd", math.NaN(), "")
	jobArg, rest, extra := splitJobArg(args, "allow-write")
	if proceed, code := parseFlags(fs, rest, stdout); !proceed {
		return code
	}
	if len(extra) > 0 {
		fmt.Fprintf(stderr, "usage error: unexpected argument %q; a run takes one job name, and each voice is passed as --with\n", extra[0])
		return envoy.ExitUsage
	}
	if jobArg == "" {
		fmt.Fprintln(stderr, "usage error: a job name is required: envoy run <job> --prompt-file <F> --with <voice>")
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
	req.Job = jobArg
	req.With = with
	req.Stdout = stdout
	req.Stderr = stderr
	return envoy.Run(req)
}

func cmdCollect(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("collect", stderr)
	resultOnly := fs.Bool("result-only", false, "")
	statusOnly := fs.Bool("status-only", false, "")
	jobArg, rest, extra := splitJobArg(args, "result-only", "status-only")
	if proceed, code := parseFlags(fs, rest, stdout); !proceed {
		return code
	}
	if len(extra) > 0 {
		fmt.Fprintln(stderr, "collect error: pass exactly one job")
		return envoy.ExitUsage
	}
	return envoy.Collect(envoy.CollectRequest{
		Job:        jobArg,
		ResultOnly: *resultOnly,
		StatusOnly: *statusOnly,
		Stdout:     stdout,
		Stderr:     stderr,
	})
}

func cmdPending(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("pending", stderr)
	base := fs.String("base", "", "")
	if proceed, code := parseFlags(fs, args, stdout); !proceed {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "collect error: pending does not take a job; use --base to choose the job root")
		return envoy.ExitUsage
	}
	return envoy.Pending(*base, stdout, stderr)
}

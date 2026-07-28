// Command envoy runs one headless AI-session turn and returns it as data.
//
//	envoy turn --provider <claude|codex> --prompt-file <F> [flags]
//	envoy collect [out-dir]
//	envoy steer --prompt-file <F> [out-dir]
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
	"github.com/qiushiyan/envoy/internal/prose"
)

// usageText is the tool description an agent reads before driving envoy, so
// it carries what the CLI cannot otherwise reveal: the dispatch/collect loop,
// what a turn leaves behind, and the facts that decide whether a retry is safe.
// A caller that reads this page should not need any other instructions to use
// envoy correctly. Effort values render from the provider package so the help
// and the validation cannot disagree.
var usageText = fmt.Sprintf(`envoy — run headless AI-session turns (claude or codex) and return them as data

USAGE
  envoy turn --provider <claude|codex> --prompt-file <F> [flags]
  envoy fan --prompt-file <F> --with <spec> --with <spec> [flags]
                              one prompt, several models, one job
  envoy fan --resume-from <fan-out-dir> --prompt-file <F> [flags]
                              continue every member of a finished fan-out
  envoy collect [flags] [job-dir]
                              print one job: status, coordinates, result.md
  envoy steer --prompt-file <F> [job-dir]
                              route a supplemental prompt to a dispatched job
  envoy pending [--base DIR]  jobs still needing attention, after a missed completion
  envoy version

THE LOOP
  1. Write the whole prompt to a file. The turn starts cold — it has none of
     your conversation — so that file carries every fact it needs.
  2. Dispatch, and let the command run to completion: minutes to hours. Run it
     as a background job; the process exiting is the completion signal.
  3. Collect. That prints the result and, when something went wrong, the one
     action to take next.

    envoy turn --provider codex --prompt-file brief.md --timeout-min 30 --label review
    # ...the turn runs; once the process exits:
    envoy collect               # newest job for this project, or pass a job dir
    envoy collect --result-only # an ok job's result body alone; a job that is
                                # not ok prints its full block — its status IS
                                # the result then
    envoy collect --status-only # everything except the result body; marks
                                # nothing collected, so the result stays owed

  Tailing the logs shows progress, never completion — a quiet log means the
  model is thinking.

WHAT A TURN LEAVES BEHIND
  One job directory, printed as `+"`out-dir:`"+` at dispatch and kept outside your
  project tree:
    result.md     the turn's answer — this is the return value
    prompt.md     exactly what was sent
    meta.json     status, session id, and recovery coordinates
    progress.log  lifecycle milestones and a heartbeat
    raw.log       the provider's own event stream, verbatim
    stderr.log    the provider's stderr

ONE PROMPT, SEVERAL MODELS
  envoy fan --prompt-file brief.md --with codex --with claude:opus --label consult

  Sends the same prompt to several turns at once and supervises them as one
  job: one command to run in the background, one completion, one collect that
  prints every member's result. Reach for it when you want independent takes on
  the same question — a second opinion from another model family, say. When the
  prompts differ, dispatch separate turns instead.

  A member is provider[:model[:effort]]:
    --with codex --with claude:opus               two families, one brief
    --with claude:opus:high --with claude:sonnet  one family, two models
    --with codex --with codex                     the same model twice

  Each member is an ordinary turn with its own session and job dir, in a
  subdirectory named after it, so recovery stays per member: resume the one
  that needs resuming. A fan-out is read-only — its members share one working
  tree, and concurrent write turns overwrite each other.

    envoy collect <fan-out-dir>   every member's status and result, in one block

  A finished fan-out continues as a set: one NEW prompt file, every member
  resumed in its own session, supervised as a new fan-out. The roster, each
  member's session, the working directory, and the baseline come from the
  original fan-out's records; collect prints this command whenever every
  member can be continued.

    envoy fan --resume-from <fan-out-dir> --prompt-file round2.md

TURN FLAGS
  --provider        claude or codex (required)
  --prompt-file     the full prompt (required); copied into the job dir
  --model           model override; omitted means the provider's config chooses
  --effort          claude: %s · codex: %s
  --resume ID       continue an existing session with a new prompt
  --allow-write     let the turn edit files and run commands unattended
  --baseline SHA    diff anchor for collect; write turns default to HEAD
  --cwd DIR         directory the provider works in (default: current dir)
  --out-dir DIR     job directory (default: the central store under ~/.local/state/envoy)
  --timeout-min N   wall-clock safety cap in minutes, 0 = off (default 30)
  --max-budget-usd  per-turn cost cap (claude only)
  --label TEXT      names the job dir (default: provider name)

  envoy fan takes --prompt-file, --baseline, --cwd, --out-dir, --timeout-min
  and --label with the same meaning, plus --with once per member. The cap
  applies to each member separately. --resume-from <fan-out-dir> replaces
  --with entirely: the members come from that fan-out's manifest.

FORGOT SOMETHING AFTER DISPATCHING
  envoy steer --prompt-file more.md [job-dir]

  No provider accepts input into a running turn — claude would queue it as a
  separate turn, codex reads its instructions once at dispatch — so nothing is
  ever injected. Steer inspects the job and answers with the one command that
  does carry the supplement: the follow-up turn that continues the same
  session, with your file already in its --prompt-file slot. On a fan-out,
  steer a member, or make the supplement a new round with fan --resume-from.
  Steer never marks anything collected; it exits 0 once it has answered.

BEFORE YOU DISPATCH
  Model and effort. Leaving --model or --effort off puts the provider's own
  configuration in charge; envoy never substitutes a model of its own, and
  reports `+"`(provider default)`"+` rather than guessing. What the provider says it
  actually ran shows up in collect.

  Write intent, not a sandbox. --allow-write lets the turn edit files and run
  commands unattended. Without it the turn is effectively read-only, and
  "analyse only, change nothing" belongs in the prompt — envoy does not
  enforce it.

  One live turn per session. A session id admits a single turn at a time; a
  second one is refused rather than allowed to interleave and corrupt the
  conversation.

  Continuing a turn. --resume <session> continues an existing conversation and
  takes a NEW prompt file: re-sending the original repeats work the provider
  already did. collect prints the exact command, carrying the settings the
  first turn was dispatched with.

  The cap. --timeout-min bounds wall-clock time as a safety net, not a stall
  detector — it counts healthy work, so reaching it never proves a hang.

EXIT CODES, AND WHAT EACH ONE LICENSES
  0 ok           result.md holds the turn's answer
  1 failed       the provider ran and reported a failure; partial work may exist
  2 infra        envoy or the environment failed, not the model
  3 usage        flags were rejected, or the session is locked; nothing ran
  4 timeout      the cap elapsed — not evidence the provider hung
  5 interrupted  a signal stopped the turn
  6 partial      fan-out only: some members returned a result and others did
                 not — the results that landed are usable, and only the members
                 that failed need a decision

  For any non-zero exit, collect the job and follow its `+"`next:`"+` line instead of
  re-dispatching. Whether the provider accepted the prompt is what decides
  between a safe retry and duplicating work that already changed the tree, and
  the job knows which happened. After a crash or restart, `+"`envoy pending`"+` finds
  the jobs whose completion you may have missed.
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
	case "fan":
		return cmdFan(args[1:], stdout, stderr)
	case "collect":
		return cmdCollect(args[1:], stdout, stderr)
	case "steer":
		return cmdSteer(args[1:], stdout, stderr)
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

// stringList collects a repeatable flag in the order it was given, which is
// the order --with members are dispatched and listed.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, " ") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdFan(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("fan", stderr)
	var req envoy.FanRequest
	var with stringList
	fs.Var(&with, "with", "")
	fs.StringVar(&req.ResumeFrom, "resume-from", "", "")
	fs.StringVar(&req.PromptFile, "prompt-file", "", "")
	fs.StringVar(&req.Baseline, "baseline", "", "")
	fs.StringVar(&req.Cwd, "cwd", "", "")
	fs.StringVar(&req.OutDir, "out-dir", "", "")
	fs.StringVar(&req.Label, "label", "", "")
	timeoutMin := fs.Float64("timeout-min", 30, "")
	// The turn-only flags are accepted here just to be refused in the fan-out's
	// own terms: a caller that reaches for --model on a fan-out should be taught
	// the member spec, not handed "flag provided but not defined".
	allowWrite := fs.Bool("allow-write", false, "")
	resume := fs.String("resume", "", "")
	model := fs.String("model", "", "")
	effort := fs.String("effort", "", "")
	providerFlag := fs.String("provider", "", "")
	if proceed, code := parseFlags(fs, args, stdout); !proceed {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage error: unexpected argument %q; each member is passed as --with %s\n",
			fs.Arg(0), "provider[:model[:effort]]")
		return envoy.ExitUsage
	}
	refusal := ""
	switch {
	case *allowWrite:
		refusal = prose.FanAllowWriteRefused()
	case *resume != "":
		refusal = prose.FanResumeRefused()
	case *model != "":
		refusal = prose.FanMemberFlagRefused("--model")
	case *effort != "":
		refusal = prose.FanMemberFlagRefused("--effort")
	case *providerFlag != "":
		refusal = prose.FanMemberFlagRefused("--provider")
	}
	if refusal != "" {
		fmt.Fprintf(stderr, "usage error: %s\n", refusal)
		return envoy.ExitUsage
	}
	if *timeoutMin == 0 {
		req.NoTimeout = true
	} else {
		req.TimeoutMin = *timeoutMin
	}
	req.With = with
	req.Stdout = stdout
	req.Stderr = stderr
	return envoy.Fan(req)
}

func cmdCollect(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("collect", stderr)
	resultOnly := fs.Bool("result-only", false, "")
	statusOnly := fs.Bool("status-only", false, "")
	if proceed, code := parseFlags(fs, args, stdout); !proceed {
		return code
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "collect error: pass at most one out-dir, with any flags before it")
		return envoy.ExitUsage
	}
	return envoy.Collect(envoy.CollectRequest{
		OutDir:     fs.Arg(0),
		ResultOnly: *resultOnly,
		StatusOnly: *statusOnly,
		Stdout:     stdout,
		Stderr:     stderr,
	})
}

func cmdSteer(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("steer", stderr)
	promptFile := fs.String("prompt-file", "", "")
	if proceed, code := parseFlags(fs, args, stdout); !proceed {
		return code
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage error: pass at most one job dir, with --prompt-file before it")
		return envoy.ExitUsage
	}
	return envoy.Steer(envoy.SteerRequest{
		PromptFile: *promptFile,
		OutDir:     fs.Arg(0),
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
		fmt.Fprintln(stderr, "collect error: pending does not take an out-dir; use --base to choose the job root")
		return envoy.ExitUsage
	}
	return envoy.Pending(*base, stdout, stderr)
}

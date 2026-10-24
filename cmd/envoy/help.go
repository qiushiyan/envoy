package main

import (
	"fmt"
	"strings"

	"github.com/qiushiyan/clikit"

	"github.com/qiushiyan/envoy/internal/provider"
)

// The pages an agent reads before driving envoy. clikit renders the usage
// lines, arguments, flags, defaults, constraints and exit codes from the
// declaration in main.go; what is here is what the declaration cannot say:
// the loop, the voice grammar, and the facts that decide whether a retry is
// safe. A section that serves several pages is one value returned by each.

func (CLI) Help() string {
	return `A turn is one prompt sent to a session of the claude or codex CLI, run
headless. envoy runs it as a job you name, in a process of its own, and leaves
its result and records in a directory you read back by that name.

The loop:
  1. Write the whole prompt to a file. The session starts cold — it has none
     of your conversation — so that file carries every fact it needs.
  2. Name the job and dispatch. Run the command in the background and let it
     run to completion (minutes to hours); the process exiting is the
     completion signal. You chose the name, so there is nothing to read back.
  3. Collect by that name. It prints the result and, when something went
     wrong, the one action to take next.

    envoy run review-r1 --prompt-file brief.md --with codex --timeout-min 30
    envoy collect review-r1

Read envoy run --help before a first dispatch: voices, continuations,
standing instructions and what each exit code licenses are there.`
}

func (CLI) HelpSections() []clikit.Section {
	return []clikit.Section{turnSection, namesSection, leftBehindSection}
}

// turnSection is the decision every command's reader faces: the waiting and
// the turn are separate, and recovery comes from the job's records.
var turnSection = clikit.Section{Title: "Your command and the turn", Body: `The turn runs in a process of its own, not inside your command. If your
session ends or the background task is stopped, only the waiting stops: the
turn runs on to its end or its cap. Run envoy wait <job> in the background to
be told again when it ends; envoy pending lists a job of yours still running.
To stop the turn itself, run the stop: command the dispatch prints as it
starts, which envoy collect --status-only <job> also prints while it runs;
Ctrl-C on the dispatch stops it too.

After any non-zero exit of a dispatch, collect the job and follow its next:
line instead of re-dispatching: whether the provider accepted the prompt
decides between a safe retry and duplicating work that already changed the
tree, and the job knows which happened. The one exception is a refusal that
ran nothing; envoy run --help says how to recognise it.`}

var namesSection = clikit.Section{Title: "Job names", Body: `A <job> is a name (one path segment; letters, digits, . _ -) kept in this
project's store under ~/.local/state/envoy, or a directory path. A name means
the latest job your session dispatched under it, so sessions that share a
checkout use the same names without reading each other's jobs. Every dispatch
runs in a directory of its own — <name>, then <name>+2, +3 — and each stays
readable by that path. A session that has dispatched nothing under a name
reads the newest job under it, which is how work from an earlier session is
picked up; the block then carries a "note:" line saying another session
dispatched it, so read that line when you expected a job of your own.

Another session's job never refuses your dispatch. Only your own earlier job
under the name does, while it is still running or uncollected — so once you
have collected a job its name is free to reuse. A round 2 takes its own name
anyway (review-r2), because that keeps round 1 readable by name.

Your session is what your harness exports: ENVOY_CALLER, else Claude Code's
CLAUDE_CODE_SESSION_ID. A caller with neither is the exception to both rules:
to it a name means the newest job of anyone's, and its jobs and everyone
else's refuse each other's dispatches under a shared name while running or
uncollected.`}

var leftBehindSection = clikit.Section{Title: "What a job leaves behind", Body: `result.md        the answer — this is the return value
prompt.md        exactly what was sent
instructions.md  the standing instructions it was sent, when its
                 conversation has them
meta.json        status, session id, and recovery coordinates
progress.log     lifecycle milestones and a heartbeat
raw.log          the provider's own event stream, verbatim
stderr.log       the provider's stderr
A fan-out holds group.json and one such directory per member, named after
the member (codex, claude-opus, codex-2 …).`}

func (RunCmd) Help() string {
	return `Dispatching prints the job's directory, its settings and a stop: command,
then blocks until the turn ends and prints how it ended. One voice is a single
turn; several are a fan-out — each voice sent its own prompt file, or
--prompt-file when it names none — supervised as one job that exits once
every member is done, and collected once.

    envoy run review-r1 --prompt-file brief.md --with codex --timeout-min 30
    envoy run consult-r1 --prompt-file brief.md --with codex --with claude:opus`
}

func (RunCmd) HelpSections() []clikit.Section {
	return []clikit.Section{
		{Title: "Voices", Body: voicesBody()},
		{Title: "Standing instructions", Body: `Rules that hold for every turn of a conversation — working unattended, what
stays out of commits — go in --instructions-file once, when the conversation
starts, instead of in each turn's prompt. The provider receives them as
system-level text, codex as its developer_instructions setting, replacing any
in its own config. Every turn that continues the conversation passes them
again from the job's records, so the session keeps them after its context is
compacted; a turn's prompt never needs to repeat them. A conversation keeps
the instructions it started with: a continuation accepts --instructions-file
only when the file matches them, and refuses a different one, or any for a
conversation that started without, because the session would go on holding
its first text. A turn's prompt can add to them for that turn, but not
reliably lift one: the model weighs system-level text above the prompt, so a
changed rule takes a new conversation.`},
		{Title: "Running providers through a launcher", Body: `ENVOY_CODEX_CMD and ENVOY_CLAUDE_CMD replace the bare provider command with a
prefix. One possible launcher is headroom:
  export ENVOY_CODEX_CMD="headroom launch --vendor codex --"
  export ENVOY_CLAUDE_CMD="headroom launch --"
Unset, empty or whitespace-only values keep the bare binary from PATH.
Prefixes split on whitespace; no shell, quoting or expansion. Paths and
arguments with spaces need a wrapper executable on PATH. Provider arguments
are appended intact. Each fresh or resumed turn reads the current setting;
fan-out members use their own provider's setting. meta.json records it as
commandPrefix. Launcher failures are infra (exit 2), with no fallback;
stderr.log preserves launcher stderr, and stderr alone does not mean failure.`},
		{Title: "What the flags cannot tell you", Body: `Model and effort. Leaving them off puts the provider's own configuration in
charge; envoy never substitutes a model of its own and reports
` + "`(provider default)`" + ` rather than guessing. The model the provider says it ran is
recorded either way; ` + "`collect --status-only`" + ` prints it, and a difference from the
request is the provider's own aliasing, not a substitution.

Write intent, not a sandbox. envoy restricts no session: claude runs with its
permission prompts bypassed, since a headless turn has no one to answer them,
and codex under its own configuration. --allow-write records the intent and
anchors the diff; any turn can still write, so "analyse only, change nothing"
belongs in the prompt. A fan-out is read-only: its members share one working
tree.

Continuing. A finished job that holds a session prints ` + "`resume:`" + `, the exact
command that continues its conversation as a new job — for a fan-out, the
whole set when every member can continue; each member's section prints its
own. It takes a NEW prompt file, since re-sending the original repeats work
the provider already did. One live turn per session: continuing a job that is
still running, or whose session may belong to another job, is refused.

The cap bounds wall-clock time as a safety net, not a stall detector — it
counts healthy work, so reaching it never proves a hang.`},
		{Title: "What each exit code licenses", Body: `A dispatch that exits 3 without ever printing a "job:" line ran nothing, so
collecting its name reads an earlier job if there is one, never this
dispatch. Its message says what to do instead: an "envoy: error:" line names
what to change (for a held name, pick another) before dispatching again; a
"lock error:" line names the job that owns the session, which is the one to
collect. For every other non-zero exit, a fan-out's 3 included, collect the
job and follow its ` + "`next:`" + ` line instead of re-dispatching. After a crash or
restart, ` + "`envoy pending`" + ` finds the jobs whose completion you may have missed,
including ones still running.

A fan-out exits 6 when some members returned a result and others did not:
the results that landed are usable, and only the members that failed need a
decision. A fan-out where no member returned a result exits with its worst
member's code, so a non-zero code says nothing about the other members:
collect it and read each member's own status.

A dispatch that receives SIGTERM or SIGHUP — your session ending, a stopped
task, timeout, kill — stops only its waiting and exits by that signal, with
none of these codes; its turn runs on, and envoy wait <job> waits for it
again. Only Ctrl-C on the dispatch and the stop: command stop the turn.`},
		turnSection,
		namesSection,
	}
}

// voicesBody renders the voice grammar, with each provider's efforts read
// from the provider registry so the page and the validation cannot disagree.
func voicesBody() string {
	var efforts []string
	for _, name := range provider.Names() {
		efforts = append(efforts, name+": "+strings.Join(provider.EffortList(name), " "))
	}
	return fmt.Sprintf(`--with provider[:model[:effort]]   a cold session
  --with codex                     the provider's own configured model
  --with claude:opus               a named model
  --with codex:gpt-6-astra:high    model and effort
  --with codex::high               effort alone; the model slot stays empty
--with @<job>                      continue that finished job's conversation
  --with @consult-r1                        round 2 in the same session
  --with @consult-r1/codex --with claude:opus
                                   one member warm beside a cold voice
  --with @consult-r1  (alone)      continue every member of a fan-out
--with <voice>=<file>              that voice's own prompt file
  --with codex=landscape.md --with claude:opus=critique.md
                                   two voices, two prompts, one job
  --with @consult-r1=round2.md     one NEW prompt to every member

Efforts — %s

Members never see each other's output. A fan-out reference counts as its
members, and it stands alone: to seat one of its members beside others, name
the member. A continued voice takes its provider, session, model, effort,
tree, baseline, write intent and standing instructions from the job's own
records; --cwd and --baseline override. Provider and model cannot change
across a continuation.`, strings.Join(efforts, " · "))
}

func (CollectCmd) Help() string {
	return `Collect prints the job as one block: its status, the result body for a turn
that produced one, and a next: line naming the one action to take. A fan-out
prints a section per member under one status. A finished job that holds a
session also prints resume:, the command that continues its conversation.

Collecting marks the job collected once the block has reached you;
--status-only never does, and neither do wait or pending. Collect exits 0
whenever it printed the job, whatever the job's status: read the block, not
the code.`
}

func (CollectCmd) HelpSections() []clikit.Section {
	return []clikit.Section{turnSection, namesSection, leftBehindSection}
}

func (WaitCmd) Help() string {
	return `Wait blocks until the job's own process has finished it, then prints the
block its dispatch prints on ending and exits with the dispatch's code. It is
how a caller that lost its dispatch — a session that ended, a background task
that was stopped — is told again when the turn ends. It delivers nothing: it
prints no result and marks nothing collected, so collect the job after it.`
}

func (WaitCmd) HelpSections() []clikit.Section {
	return []clikit.Section{turnSection, namesSection}
}

func (PendingCmd) Help() string {
	return `Pending is the recovery index for a completion you may have missed, after a
compaction, a crash or a restart: it lists each job of yours still running or
finished but uncollected, with why it is listed and the next action. It prints
no results and marks nothing collected. A store that cannot be read is
reported as such, never as a store with no jobs.`
}

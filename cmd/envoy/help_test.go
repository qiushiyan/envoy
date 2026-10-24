package main

import (
	"strings"
	"testing"

	"github.com/qiushiyan/clikit/clikittest"
)

// TestHelpPages pins every page byte for byte; make golden accepts an
// intended change.
func TestHelpPages(t *testing.T) {
	clikittest.Golden(t, app, "testdata/help")
}

// The pages are the tool description a caller reads before driving envoy, so
// together they have to stand alone: the loop, the voice grammar, what a job
// leaves behind, the facts that decide whether a retry is safe, and what each
// exit code licenses. Each fact is checked on the page that owns it, and the
// root page sends a first dispatch to run's — a caller who has read them
// should need no other instructions.
func TestHelpPagesTeach(t *testing.T) {
	pages := map[string][]string{
		"": {
			"The loop:",               // name → dispatch → collect
			"envoy collect review-r1", // the read path, by name
			"nothing to read back",    // why the name is chosen up front
			"Read envoy run --help before a first dispatch",
			"means the latest job your session dispatched under it", // what a reused name means
			"Another session's job never refuses your dispatch",     // a name that fell back is visible
			"ENVOY_CALLER, else Claude",                             // where the session identity comes from
			"<name>+2",                                              // so a +N directory is not read as a fault
			"envoy wait <job>",                                      // the wait that restores the completion signal
			"only the waiting stops",                                // a stopped task no longer stops the turn
			"run the stop: command",                                 // so the turn is stopped this way
			"follow its next:",                                      // recovery comes from the job's records
			"instructions.md  the standing instructions it was sent",
		},
		"run": {
			"--with=VOICE[=FILE]",
			"--with codex::high",                          // effort without a model
			"--with @<job>",                               // continuation
			"--with @consult-r1/codex --with claude:opus", // warm beside cold
			"stands alone",                                // a fan-out reference
			"--with <voice>=<file>",                       // a voice's own prompt
			"--with @consult-r1=round2.md",                // a round on one NEW prompt
			"claude: low medium high xhigh max",           // rendered from the provider map
			"--instructions-file=FILE",
			"write intent and standing instructions from the job's own records", // carried on by a continuation
			"so the session keeps them after its context is compacted",          // why they ride every turn
			"never needs to repeat them",
			"and refuses a different one",
			"but not reliably lift one",
			"replacing any in its own config", // codex's developer_instructions
			`ENVOY_CODEX_CMD="headroom launch --vendor codex --"`,
			`ENVOY_CLAUDE_CMD="headroom launch --"`,
			"no shell, quoting or expansion",
			"Each fresh or resumed turn reads the current setting",
			"commandPrefix",
			"with no fallback",
			"envoy never substitutes", // no model substitution
			"not a sandbox",           // --allow-write is intent
			"One live turn per session",
			"takes a NEW prompt file",                                // resume discipline
			"counts healthy work",                                    // cap semantics
			`without ever printing a "job:" line ran nothing`,        // the observable mark of a refusal that ran nothing
			"reads an earlier job if there is one",                   // and why such a dispatch is never collected
			`an "envoy: error:" line names`,                          // the refusal's line
			"for a held name, pick another",                          // the one collision a caller hears about
			`"lock error:" line names the job that owns the session`, // a held session says where to look
			"fan-out's 3 included",                                   // but a fan-out's exit 3 may have run members
			"including ones still running",                           // pending lists a turn whose waiter is gone
			"stops only its waiting and exits by that signal",        // timeout, kill and scripts meet this too
			"6    partial",
			"only the waiting stops",
		},
		"collect": {
			"--status-only never does",
			"exits 0\nwhenever it printed the job",
			"resume:",
		},
		"wait": {
			"It delivers nothing",
			"only the waiting stops",
		},
		"pending": {
			"never as a store with no jobs",
		},
	}
	for page, phrases := range pages {
		args := append(strings.Fields(page), "--help")
		r := clikittest.Run(app, args...)
		if r.Code != 0 {
			t.Fatalf("%v: exit %d\n%s", args, r.Code, r.Stderr)
		}
		// Prose wraps where it likes; a phrase is read across line breaks.
		text := strings.Join(strings.Fields(r.Stdout), " ")
		for _, p := range phrases {
			if !strings.Contains(text, strings.Join(strings.Fields(p), " ")) {
				t.Errorf("envoy %s --help lacks %q", page, p)
			}
		}
	}
}

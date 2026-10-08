package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The command line itself: the help page an agent reads first, usage
// errors, and the rosters refused before anything is reserved.

// A roster that cannot mean anything is refused in its own terms, before
// any voice spawns and without a job directory left behind.
func TestRunRefusesMalformedRosters(t *testing.T) {
	e := newEnv(t)
	prompt := writePrompt(t, t.TempDir())
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--with", "codex", "--with", "claude", "--allow-write"}, "--allow-write needs exactly one --with"},
		{[]string{"--with", "codex", "--with", "gemini"}, "must name provider claude or codex"},
		{[]string{"--with", "codex", "--with", "claude:opus:minimal"}, "claude has no 'minimal'"},
		{[]string{"--with", "codex", "--with", "claude:opus:high:extra"}, "has too many fields"},
		{[]string{"--with", "codex", "--with", "claude", "codex"}, "unexpected argument"},
		{[]string{"--with", "codex", "--model", "opus"}, "flag provided but not defined: -model"},
	}
	for _, c := range cases {
		dir := filepath.Join(t.TempDir(), "job")
		res := runEnvoy(t, e, runArgs(prompt, dir, c.args...)...)
		if res.code != 3 {
			t.Fatalf("%v: exit = %d, want 3\nstderr:\n%s", c.args, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stderr of %v", c.args), res.stderr, c.want)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%v: a refused run must leave no job dir", c.args)
		}
	}
}

// An explicit help request is not a usage error: agents read exit codes.
//
// The page is also the tool description a caller reads before driving envoy,
// so it has to stand alone: the loop, the voice grammar, what a job leaves
// behind, the facts that decide whether a retry is safe, and what each exit
// code licenses. A caller that has read this should need no other
// instructions — and nothing on it addresses a human at a keyboard.
func TestHelpIsSelfSufficient(t *testing.T) {
	res := runEnvoy(t, newEnv(t), "collect", "--help")
	if res.code != 0 {
		t.Fatalf("collect --help exit = %d, want 0\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"envoy run <job> [--prompt-file <F>] --with <voice>[=<F>]", // the dispatch form
		"THE LOOP",                // name → dispatch → collect
		"envoy collect review-r1", // the read path, by name
		"nothing to read back",    // why the name is chosen up front
		"means the latest job your session dispatched under it", // what a reused name means
		"Another session's job never refuses your dispatch",     // a name that fell back is visible
		"ENVOY_CALLER, else Claude",                             // where the session identity comes from
		"<name>+2",                                              // so a +N directory is not read as a fault
		"without ever printing a \"job:\" line ran nothing",     // the observable mark of a refusal that ran nothing
		"reads an earlier job if there is one",                  // and why such a dispatch is never collected
		"names the job that owns the session",                   // a held session is one of them, and says where to look
		"for a held name, pick another",                         // the one collision a caller hears about
		"fan-out's 3 included",                                  // but a fan-out's exit 3 may have run members
		"VOICES",
		"--with codex::high",                          // effort without a model
		"--with @<job>",                               // continuation
		"--with @consult-r1/codex --with claude:opus", // warm beside cold
		"stands alone",                                // a fan-out reference
		"--with <voice>=<file>",                       // a voice's own prompt
		"--with @consult-r1=round2.md",                // a round on one NEW prompt
		"WHAT A JOB LEAVES BEHIND",
		"RUNNING PROVIDERS THROUGH A LAUNCHER",
		"ENVOY_CODEX_CMD=\"headroom launch --vendor codex --\"",
		"ENVOY_CLAUDE_CMD=\"headroom launch --\"",
		"no shell, quoting or expansion",
		"Each fresh or resumed turn reads the current setting",
		"commandPrefix",
		"with no fallback",
		"FACTS THE FLAGS CANNOT TELL YOU",
		"envoy never substitutes", // no model substitution
		"not a sandbox",           // --allow-write is intent
		"One live turn per session",
		"takes a NEW prompt file",                         // resume discipline
		"counts healthy work",                             // cap semantics
		"envoy wait <job>",                                // the wait that restores the completion signal
		"only the waiting stops",                          // a stopped task no longer stops the turn
		"run the stop: command",                           // so the turn is stopped this way
		"including ones still running",                    // pending lists a turn whose waiter is gone
		"stops only its waiting and exits by that signal", // timeout, kill and scripts meet this too
		"EXIT CODES OF A RUN, AND WHAT EACH ONE LICENSES",
		"6 partial",
		"claude: low medium high xhigh max", // rendered from the provider map
	)
}

func TestUsageErrors(t *testing.T) {
	e := newEnv(t)
	prompt := writePrompt(t, t.TempDir())
	job := filepath.Join(t.TempDir(), "job")
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"run", "--prompt-file", prompt, "--with", "codex"}, "a job name is required"},
		{[]string{"run", "bad name", "--prompt-file", prompt, "--with", "codex"}, "one segment"},
		{[]string{"run", job, "--prompt-file", prompt}, "at least one --with is required"},
		{[]string{"run", job, "--with", "gemini", "--prompt-file", prompt}, "must name provider claude or codex"},
		{[]string{"run", job, "--with", "codex"}, "--with codex has no prompt"},
		{[]string{"run", job, "--with", "codex=", "--prompt-file", prompt}, "give the prompt file after '='"},
		{[]string{"run", job, "--with", "codex=" + filepath.Join(t.TempDir(), "absent.md")}, "prompt file not found"},
		{[]string{"run", job, "--with", "codex", "--prompt-file", t.TempDir()}, "is a directory, not a file"},
		{[]string{"run", job, "--with", "claude::minimal", "--prompt-file", prompt}, "claude has no 'minimal'"},
		{[]string{"run", job, "--with", "codex", "--prompt-file", prompt, "--timeout-min", "-1"}, "--timeout-min must be a number >= 0"},
		{[]string{"run", job, "--with", "codex", "--prompt-file", prompt, "--max-budget-usd", "1"}, "caps one claude voice"},
		{[]string{"run", job, "--with", "claude", "--with", "codex", "--prompt-file", prompt, "--max-budget-usd", "1"}, "caps one claude voice"},
		{[]string{"nonsense"}, "unknown command"},
	}
	for _, c := range cases {
		res := runEnvoy(t, e, c.args...)
		if res.code != 3 {
			t.Fatalf("%v: exit = %d, want 3\nstderr:\n%s", c.args, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stderr of %v", c.args), res.stderr, c.want)
	}
	if _, err := os.Stat(job); !os.IsNotExist(err) {
		t.Fatal("no refused run may leave a job dir behind")
	}
}

// A roster is refused whole before its name is reserved: one voice with no
// prompt from either source, or one member's file that cannot be read, must
// not start its siblings.
func TestRunRefusesAnUnpromptedVoiceBeforeReserving(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--with", "codex=" + prompt, "--with", "claude:opus"}, "--with claude:opus has no prompt"},
		{[]string{"--with", "codex=" + prompt, "--with", "claude:opus=" + filepath.Join(t.TempDir(), "absent.md")}, "prompt file not found"},
		{[]string{"--with", "codex", "--with", "claude:opus=" + prompt, "--prompt-file", filepath.Join(t.TempDir(), "absent.md")}, "prompt file not found"},
	}
	for _, c := range cases {
		dir := filepath.Join(t.TempDir(), "group")
		res := runEnvoy(t, e, append([]string{"run", dir}, c.args...)...)
		if res.code != 3 {
			t.Fatalf("%v: exit = %d, want 3\nstderr:\n%s", c.args, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stderr of %v", c.args), res.stderr, c.want)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%v: a refused roster must leave no job dir", c.args)
		}
	}
}

// '=' splits a voice from its prompt file at the first occurrence, so a prompt
// path may contain one. The only spelling that could carry '=' on the voice
// side is a job named by a directory path, and that is refused by name rather
// than split into a job and a file the caller never meant.
func TestEqualsInPathsIsSplitOnceAndRefusedInJobPaths(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	src := t.TempDir()
	odd := filepath.Join(src, "a=b.md")
	os.WriteFile(odd, []byte("odd prompt body\n"), 0o644)

	outDir := filepath.Join(t.TempDir(), "job")
	res := runEnvoy(t, e, "run", outDir, "--with", "codex="+odd, "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if got := readFile(t, filepath.Join(outDir, "prompt.md")); got != "odd prompt body\n" {
		t.Fatalf("prompt.md = %q", got)
	}

	// A job that lives at a path with '=' cannot be continued by that path.
	oddJob := filepath.Join(t.TempDir(), "review=old")
	if r := runEnvoy(t, e, runArgs(odd, oddJob, "--with", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("odd job exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	r2 := filepath.Join(t.TempDir(), "round2")
	res = runEnvoy(t, e, runArgs(odd, r2, "--with", "@"+oddJob)...)
	if res.code != 3 {
		t.Fatalf("continuing a '=' path: exit = %d, want 3\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "a job directory path may not contain '='")
	if _, err := os.Stat(r2); !os.IsNotExist(err) {
		t.Fatal("a refused continuation must leave no job dir")
	}
}

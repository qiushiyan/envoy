// Cross-phase continuation: a voice spelled @<job> anchors a follow-up on a
// finished job's records, alone or beside cold voices in one fan-out. The
// records are the coordinate — the session id, settings, and tree come from
// meta.json, never from what the caller happens to remember — and every
// dispatch records the lineage it was given.
package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A follow-up dispatched as @<job> must run as a real resume of the recorded
// session, inherit the recorded settings (model, effort, cwd), take the cap
// from this dispatch (the cap is phase policy), and record where the
// conversation came from.
func TestTurnResumeFromContinuesSession(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID", "sess-consult")
	workDir := t.TempDir()
	r1 := filepath.Join(t.TempDir(), "consult")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, r1, "--with", "codex:fake-m:high", "--cwd", workDir, "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("consult exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}

	review := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(review, []byte("review prompt body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := filepath.Join(t.TempDir(), "review")
	res = runEnvoy(t, e, "run", r2, "--with", "@"+r1, "--prompt-file", review, "--timeout-min", "7")
	if res.code != 0 {
		t.Fatalf("resume-from exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "resume-from stdout", res.stdout,
		"resumed-from: "+r1,
		// Inherited from the records; the cap stays this dispatch's own.
		"provider: codex · model fake-m · effort high · hard cap 7m",
	)

	meta := readMeta(t, r2)
	if meta["resumedFrom"] != r1 {
		t.Fatalf("resumedFrom = %v, want %s", meta["resumedFrom"], r1)
	}
	mustContain(t, "argv", fmt.Sprintf("%v", meta["providerArgv"]), "resume", "sess-consult")
	if meta["model"] != "fake-m" || meta["effort"] != "high" {
		t.Fatalf("inherited settings = model %v effort %v", meta["model"], meta["effort"])
	}
	if meta["cwd"] != workDir {
		t.Fatalf("cwd = %v, want the original turn's tree %s", meta["cwd"], workDir)
	}
	if meta["timeoutMin"] != float64(7) {
		t.Fatalf("timeoutMin = %v, want this dispatch's own 7", meta["timeoutMin"])
	}
	if got := readFile(t, filepath.Join(r2, "prompt.md")); got != "review prompt body\n" {
		t.Fatalf("prompt.md = %q, want the NEW prompt", got)
	}

	// Collect surfaces the lineage next to the session coordinates — the
	// session itself arriving inside the follow-up command that carries it.
	col := runEnvoy(t, e, "collect", "--status-only", r2)
	mustContain(t, "collect stdout", col.stdout, "resumed-from: "+r1,
		"resume: envoy run <new-job-name> --with @'"+r2+"' --timeout-min 7")
}

// Every refusal names the conflict and hands over the caller's actual next
// move; a job that cannot be continued is refused before anything spawns.
func TestContinuationRefusals(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())

	fanDir := filepath.Join(t.TempDir(), "fan")
	if r := runEnvoy(t, e, runArgs(prompt, fanDir, "--with", "codex", "--with", "claude",
		"--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("fan exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	noSession := filepath.Join(t.TempDir(), "dead")
	eDead := newEnv(t).set("ENVOY_FAKE_SCENARIO", "exit-before-stdin")
	if r := runEnvoy(t, eDead, runArgs(prompt, noSession, "--with", "codex", "--timeout-min", "5")...); r.code != 2 {
		t.Fatalf("no-session turn exit = %d, want 2\nstderr:\n%s", r.code, r.stderr)
	}

	cases := []struct {
		with []string
		want []string
	}{
		{[]string{"@" + filepath.Join(t.TempDir(), "nowhere")},
			[]string{"no job found there"}},
		{[]string{"@" + fanDir, "codex"},
			[]string{"stands alone", "--with @'" + filepath.Join(fanDir, "codex") + "'  (codex)"}},
		{[]string{"@" + noSession},
			[]string{"never published a session id", "envoy collect"}},
	}
	for _, c := range cases {
		dir := filepath.Join(t.TempDir(), "next")
		args := runArgs(prompt, dir)
		for _, w := range c.with {
			args = append(args, "--with", w)
		}
		res := runEnvoy(t, e, args...)
		if res.code != 3 {
			t.Fatalf("%v: exit = %d, want 3\nstderr:\n%s", c.with, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stderr of %v", c.with), res.stderr, c.want...)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%v: a refused run must leave no job dir", c.with)
		}
	}
}

// A mixed roster is one dispatch: the continued voice resumes its recorded
// session with its recorded settings, the cold voice starts fresh, and the
// fan-out runs in the continued conversation's tree unless --cwd overrides.
func TestFanWithFromMixedRoster(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "sess-consult").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "sess-fresh")
	workDir := t.TempDir()
	r1 := filepath.Join(t.TempDir(), "consult")
	prompt := writePrompt(t, t.TempDir())

	if r := runEnvoy(t, e, runArgs(prompt, r1, "--with", "codex:fake-m",
		"--cwd", workDir, "--baseline", "bl-consult", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("consult exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	review := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(review, []byte("review prompt body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(t.TempDir(), "review-fan")
	res := runEnvoy(t, e, "run", g, "--prompt-file", review, "--with", "@"+r1,
		"--with", "claude:opus", "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("fan exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "fan stdout", res.stdout,
		"member codex-fake-m: model fake-m · effort (provider default) · dir "+filepath.Join(g, "codex-fake-m")+" · continues "+r1,
		"member claude-opus: ",
		"status: ok — all 2 turns returned a result",
	)

	warm := readMeta(t, filepath.Join(g, "codex-fake-m"))
	mustContain(t, "warm argv", fmt.Sprintf("%v", warm["providerArgv"]), "resume", "sess-consult")
	if warm["resumedFrom"] != r1 || warm["model"] != "fake-m" {
		t.Fatalf("warm member = resumedFrom %v model %v", warm["resumedFrom"], warm["model"])
	}
	cold := readMeta(t, filepath.Join(g, "claude-opus"))
	coldArgv := fmt.Sprintf("%v", cold["providerArgv"])
	if !strings.Contains(coldArgv, "--session-id") || strings.Contains(coldArgv, "--resume") {
		t.Fatalf("cold member must start a fresh session, argv = %s", coldArgv)
	}
	if _, has := cold["resumedFrom"]; has {
		t.Fatalf("cold member must record no lineage, got %v", cold["resumedFrom"])
	}

	// The fan-out ran where the continued conversation lives, against the
	// anchor it was dispatched with.
	group := readGroup(t, g)
	if group["cwd"] != workDir {
		t.Fatalf("group cwd = %v, want the continued job's tree %s", group["cwd"], workDir)
	}
	if group["gitBaseline"] != "bl-consult" {
		t.Fatalf("gitBaseline = %v, want bl-consult inherited from the warm source", group["gitBaseline"])
	}

	col := runEnvoy(t, e, "collect", g)
	mustContain(t, "collect stdout", col.stdout,
		"=== member codex-fake-m ===",
		"resumed-from: "+r1,
		"=== member claude-opus ===",
	)
}

// Roster refusals: the shapes that cannot mean anything are stopped with the
// command that does, before any member spawns.
func TestRosterRefusals(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())

	r1 := filepath.Join(t.TempDir(), "consult")
	if r := runEnvoy(t, e, runArgs(prompt, r1, "--with", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("turn exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	// A source that ran with write intent: continuing it inside a read-only
	// fan-out would silently narrow the conversation's write intent.
	writeJob := filepath.Join(t.TempDir(), "delegate")
	if r := runEnvoy(t, e, runArgs(prompt, writeJob, "--with", "codex", "--allow-write",
		"--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("write turn exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"--with", "@" + r1, "--with", "@" + r1},
			[]string{"same conversation twice"}},
		{[]string{"--with", "@" + writeJob, "--with", "codex"},
			[]string{"read-only", "envoy run <new-job-name> --with @'" + writeJob + "'"}},
		{[]string{"--with", "@" + r1, "--with", "codex", "--allow-write"},
			[]string{"--allow-write needs exactly one --with"}},
	}
	for _, c := range cases {
		dir := filepath.Join(t.TempDir(), "next")
		res := runEnvoy(t, e, runArgs(prompt, dir, c.args...)...)
		if res.code != 3 {
			t.Fatalf("%v: exit = %d, want 3\nstderr:\n%s", c.args, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stderr of %v", c.args), res.stderr, c.want...)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%v: a refused run must leave no job dir", c.args)
		}
	}

	// Alone, the write conversation keeps its intent without being asked.
	alone := filepath.Join(t.TempDir(), "delegate-r2")
	res := runEnvoy(t, e, runArgs(prompt, alone, "--with", "@"+writeJob, "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("lone write continuation exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if readMeta(t, alone)["allowWrite"] != true {
		t.Fatal("a continued write conversation must keep its write intent")
	}
}

// Collect and dispatch share one definition of "this session may continue":
// while a turn records status running, collect must not advertise the resume
// command that --resume-from refuses for the same job.
func TestRunningJobIsNotAdvertisedAsContinuable(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	meta := fmt.Sprintf(`{"schemaVersion":6,"status":"running","provider":"codex","sessionId":"sess-live",`+
		`"cwd":%q,"promptState":"accepted","timeoutMin":5,"runnerPid":%d,"nextAction":"wait","resultKind":"none","collectedAt":null}`,
		dir, os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	col := runEnvoy(t, e, "collect", dir)
	mustContain(t, "collect stdout", col.stdout, "status: running (live", "session: sess-live")
	if strings.Contains(col.stdout, "resume: envoy run") {
		t.Fatalf("a running turn must not advertise a resume command:\n%s", col.stdout)
	}

	prompt := writePrompt(t, t.TempDir())
	res := runEnvoy(t, e, runArgs(prompt, filepath.Join(t.TempDir(), "next"), "--with", "@"+dir)...)
	if res.code != 3 {
		t.Fatalf("continuing a running job: exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "still records status running")
	if strings.Contains(res.stderr, "resume") && strings.Contains(res.stderr, "sess-live") {
		t.Fatalf("the refusal must not advertise a blocked turn's session:\n%s", res.stderr)
	}
}

// A warm source's recorded baseline is part of the conversation being
// continued: it inherits like cwd does, and two warm sources that disagree
// are refused rather than silently anchored to one of them.
func TestFanWithFromBaselines(t *testing.T) {
	prompt := writePrompt(t, t.TempDir())
	shared := t.TempDir()

	a := filepath.Join(t.TempDir(), "job-a")
	eA := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_SESSION_ID", "sess-bl-a")
	if r := runEnvoy(t, eA, runArgs(prompt, a, "--with", "codex", "--cwd", shared,
		"--baseline", "bl-a", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("job-a exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	b := filepath.Join(t.TempDir(), "job-b")
	eB := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_SESSION_ID", "sess-bl-b")
	if r := runEnvoy(t, eB, runArgs(prompt, b, "--with", "codex", "--cwd", shared,
		"--baseline", "bl-b", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("job-b exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	res := runEnvoy(t, eA, runArgs(prompt, filepath.Join(t.TempDir(), "g0"), "--with", "@"+a, "--with", "@"+b)...)
	if res.code != 3 {
		t.Fatalf("divergent baselines: exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "different baselines", "--baseline")

	// An explicit anchor resolves it.
	g := filepath.Join(t.TempDir(), "group")
	res = runEnvoy(t, eA, runArgs(prompt, g, "--with", "@"+a, "--with", "@"+b,
		"--baseline", "bl-x", "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("explicit-baseline exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if readGroup(t, g)["gitBaseline"] != "bl-x" {
		t.Fatalf("gitBaseline = %v, want the explicit bl-x", readGroup(t, g)["gitBaseline"])
	}
}

// Two continued conversations from different trees cannot share one fan-out
// silently: the engine will not choose which tree the members run in.
func TestFanWithFromRefusesMixedTrees(t *testing.T) {
	prompt := writePrompt(t, t.TempDir())
	cwdA, cwdB := t.TempDir(), t.TempDir()

	a := filepath.Join(t.TempDir(), "job-a")
	eA := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_SESSION_ID", "sess-a")
	if r := runEnvoy(t, eA, runArgs(prompt, a, "--with", "codex", "--cwd", cwdA, "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("job-a exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	b := filepath.Join(t.TempDir(), "job-b")
	eB := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_SESSION_ID", "sess-b")
	if r := runEnvoy(t, eB, runArgs(prompt, b, "--with", "codex", "--cwd", cwdB, "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("job-b exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	res := runEnvoy(t, eA, runArgs(prompt, filepath.Join(t.TempDir(), "g0"), "--with", "@"+a, "--with", "@"+b)...)
	if res.code != 3 {
		t.Fatalf("mixed-tree exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "different working directories", "pass --cwd")

	// Naming the tree resolves it.
	g := filepath.Join(t.TempDir(), "group")
	res = runEnvoy(t, eA, runArgs(prompt, g, "--with", "@"+a, "--with", "@"+b,
		"--cwd", cwdA, "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("explicit-cwd exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
}

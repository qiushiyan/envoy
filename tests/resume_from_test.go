// Cross-phase continuation: --resume-from anchors a follow-up turn on a
// finished job's records, and --with-from puts a continued conversation
// beside cold members in one fan-out. The records are the coordinate — the
// session id, settings, and tree come from meta.json, never from what the
// caller happens to remember — and every dispatch records the lineage it was
// given.
package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A follow-up dispatched with --resume-from must run as a real resume of the
// recorded session, inherit the settings the caller left unsaid (model,
// effort, cwd), obey the explicit flags (the cap is phase policy), and record
// where the conversation came from.
func TestTurnResumeFromContinuesSession(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID", "sess-consult")
	workDir := t.TempDir()
	r1 := filepath.Join(t.TempDir(), "consult")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, turnArgs(prompt, r1, "--provider", "codex", "--model", "fake-m",
		"--effort", "high", "--cwd", workDir, "--timeout-min", "5", "--label", "consult")...)
	if res.code != 0 {
		t.Fatalf("consult exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}

	review := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(review, []byte("review prompt body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := filepath.Join(t.TempDir(), "review")
	res = runEnvoy(t, e, "turn", "--resume-from", r1, "--prompt-file", review,
		"--out-dir", r2, "--timeout-min", "7", "--label", "review")
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
	mustContain(t, "collect stdout", col.stdout, "resumed-from: "+r1, "--resume sess-consult")

	// An explicit flag overrides the record; the rest still inherits.
	r3 := filepath.Join(t.TempDir(), "review-xhigh")
	res = runEnvoy(t, e, "turn", "--resume-from", r1, "--prompt-file", review,
		"--out-dir", r3, "--effort", "xhigh", "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("override exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	meta = readMeta(t, r3)
	if meta["effort"] != "xhigh" || meta["model"] != "fake-m" {
		t.Fatalf("override = model %v effort %v, want fake-m/xhigh", meta["model"], meta["effort"])
	}
}

// Every refusal names the conflict and hands over the caller's actual next
// move; a job that cannot be continued is refused before anything spawns.
func TestTurnResumeFromRefusals(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())

	fanDir := filepath.Join(t.TempDir(), "fan")
	if r := runEnvoy(t, e, fanArgs(prompt, fanDir, "--with", "codex", "--with", "claude",
		"--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("fan exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	noSession := filepath.Join(t.TempDir(), "dead")
	eDead := newEnv(t).set("ENVOY_FAKE_SCENARIO", "exit-before-stdin")
	if r := runEnvoy(t, eDead, turnArgs(prompt, noSession, "--provider", "codex", "--timeout-min", "5")...); r.code != 2 {
		t.Fatalf("no-session turn exit = %d, want 2\nstderr:\n%s", r.code, r.stderr)
	}

	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"turn", "--resume-from", t.TempDir(), "--resume", "sess-1", "--prompt-file", prompt},
			[]string{"mutually exclusive"}},
		{[]string{"turn", "--resume-from", t.TempDir(), "--provider", "codex", "--prompt-file", prompt},
			[]string{"Drop --provider"}},
		{[]string{"turn", "--resume-from", filepath.Join(t.TempDir(), "nowhere"), "--prompt-file", prompt},
			[]string{"no turn found there"}},
		{[]string{"turn", "--resume-from", fanDir, "--prompt-file", prompt},
			[]string{"this is a fan-out", "envoy fan --resume-from"}},
		{[]string{"turn", "--resume-from", noSession, "--prompt-file", prompt},
			[]string{"never published a session id", "envoy collect"}},
	}
	for _, c := range cases {
		res := runEnvoy(t, e, c.args...)
		if res.code != 3 {
			t.Fatalf("%v: exit = %d, want 3\nstderr:\n%s", c.args, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stderr of %v", c.args), res.stderr, c.want...)
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

	if r := runEnvoy(t, e, turnArgs(prompt, r1, "--provider", "codex", "--model", "fake-m",
		"--cwd", workDir, "--baseline", "bl-consult", "--timeout-min", "5", "--label", "consult")...); r.code != 0 {
		t.Fatalf("consult exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	review := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(review, []byte("review prompt body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(t.TempDir(), "review-fan")
	res := runEnvoy(t, e, "fan", "--prompt-file", review, "--with-from", r1,
		"--with", "claude:opus", "--out-dir", g, "--timeout-min", "5", "--label", "review")
	if res.code != 0 {
		t.Fatalf("fan exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "fan stdout", res.stdout,
		"member codex-fake-m: model fake-m · effort (provider default) · out-dir "+filepath.Join(g, "codex-fake-m")+" · continues "+r1,
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
func TestFanWithFromRefusals(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())

	r1 := filepath.Join(t.TempDir(), "consult")
	if r := runEnvoy(t, e, turnArgs(prompt, r1, "--provider", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("turn exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	fanDir := filepath.Join(t.TempDir(), "fan")
	if r := runEnvoy(t, e, fanArgs(prompt, fanDir, "--with", "codex", "--with", "claude",
		"--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("fan exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	// A source that ran with write intent: continuing it inside a read-only
	// fan-out would silently narrow the conversation's write intent.
	writeJob := filepath.Join(t.TempDir(), "delegate")
	if r := runEnvoy(t, e, turnArgs(prompt, writeJob, "--provider", "codex", "--allow-write",
		"--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("write turn exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"fan", "--resume-from", fanDir, "--with-from", r1, "--prompt-file", prompt},
			[]string{"mutually exclusive"}},
		{[]string{"fan", "--with-from", r1, "--prompt-file", prompt},
			[]string{"at least two members", "envoy turn --resume-from"}},
		{[]string{"fan", "--with-from", r1, "--with-from", r1, "--prompt-file", prompt},
			[]string{"same conversation twice"}},
		{[]string{"fan", "--with-from", fanDir, "--with", "codex", "--prompt-file", prompt},
			[]string{"this is a fan-out"}},
		// A lone --with-from that names no job must report that, not hand
		// over a turn command that would only fail the same way.
		{[]string{"fan", "--with-from", filepath.Join(t.TempDir(), "nowhere"), "--prompt-file", prompt},
			[]string{"no turn found there"}},
		{[]string{"fan", "--with-from", writeJob, "--with", "codex", "--prompt-file", prompt},
			[]string{"read-only", "envoy turn --resume-from"}},
	}
	for _, c := range cases {
		res := runEnvoy(t, e, c.args...)
		if res.code != 3 {
			t.Fatalf("%v: exit = %d, want 3\nstderr:\n%s", c.args, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stderr of %v", c.args), res.stderr, c.want...)
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
	if strings.Contains(col.stdout, "resume: envoy turn") {
		t.Fatalf("a running turn must not advertise a resume command:\n%s", col.stdout)
	}

	prompt := writePrompt(t, t.TempDir())
	res := runEnvoy(t, e, "turn", "--resume-from", dir, "--prompt-file", prompt)
	if res.code != 3 {
		t.Fatalf("resume-from a running job: exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "still records status running")

	// The fan redirect is a continuation surface too: aimed at this running
	// turn it must point at collect, not hand over the resume command every
	// other surface is refusing.
	res = runEnvoy(t, e, "fan", "--resume-from", dir, "--prompt-file", prompt)
	if res.code != 3 {
		t.Fatalf("fan --resume-from a running turn: exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "is a single turn", "Collect it to see what it licenses")
	if strings.Contains(res.stderr, "--resume sess-live") {
		t.Fatalf("the redirect must not advertise a blocked turn's resume command:\n%s", res.stderr)
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
	if r := runEnvoy(t, eA, turnArgs(prompt, a, "--provider", "codex", "--cwd", shared,
		"--baseline", "bl-a", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("job-a exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	b := filepath.Join(t.TempDir(), "job-b")
	eB := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_SESSION_ID", "sess-bl-b")
	if r := runEnvoy(t, eB, turnArgs(prompt, b, "--provider", "codex", "--cwd", shared,
		"--baseline", "bl-b", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("job-b exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	res := runEnvoy(t, eA, "fan", "--with-from", a, "--with-from", b, "--prompt-file", prompt)
	if res.code != 3 {
		t.Fatalf("divergent baselines: exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "different baselines", "--baseline")

	// An explicit anchor resolves it.
	g := filepath.Join(t.TempDir(), "group")
	res = runEnvoy(t, eA, "fan", "--with-from", a, "--with-from", b, "--prompt-file", prompt,
		"--baseline", "bl-x", "--out-dir", g, "--timeout-min", "5")
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
	if r := runEnvoy(t, eA, turnArgs(prompt, a, "--provider", "codex", "--cwd", cwdA, "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("job-a exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	b := filepath.Join(t.TempDir(), "job-b")
	eB := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_SESSION_ID", "sess-b")
	if r := runEnvoy(t, eB, turnArgs(prompt, b, "--provider", "codex", "--cwd", cwdB, "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("job-b exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	res := runEnvoy(t, eA, "fan", "--with-from", a, "--with-from", b, "--prompt-file", prompt)
	if res.code != 3 {
		t.Fatalf("mixed-tree exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "different working directories", "pass --cwd")

	// Naming the tree resolves it.
	g := filepath.Join(t.TempDir(), "group")
	res = runEnvoy(t, eA, "fan", "--with-from", a, "--with-from", b, "--prompt-file", prompt,
		"--cwd", cwdA, "--out-dir", g, "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("explicit-cwd exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
}

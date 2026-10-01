package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func jobLineOf(stdout string) string {
	for line := range strings.SplitSeq(stdout, "\n") {
		if after, ok := strings.CutPrefix(line, "job: "); ok {
			return after
		}
	}
	return ""
}

// A name keeps passing on, generation after generation, and a job that ended
// badly releases its name on collection exactly as an ok one does: delivery,
// not success, is what frees a name.
func TestANamePassesOnPastTheSecondGenerationAndAfterAFailure(t *testing.T) {
	ok := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	dispatch := func(e *env) runResult {
		return runEnvoyIn(t, e, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	}
	first := dispatch(ok)
	base := jobLineOf(first.stdout)
	if first.code != 0 || base == "" {
		t.Fatalf("first dispatch = %d\n%s", first.code, first.stderr)
	}
	runEnvoyIn(t, ok, project, "collect", "consult-r1")

	failing := &env{home: ok.home, extra: map[string]string{"ENVOY_FAKE_SCENARIO": "partial-failure"}}
	failed := runEnvoyIn(t, failing, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "claude", "--timeout-min", "5")
	if failed.code != 1 || jobLineOf(failed.stdout) != base+"+2" {
		t.Fatalf("second dispatch = %d in %q, want a failed turn in %s\n%s", failed.code, jobLineOf(failed.stdout), base+"+2", failed.stderr)
	}
	runEnvoyIn(t, ok, project, "collect", "consult-r1")

	third := dispatch(ok)
	if third.code != 0 || jobLineOf(third.stdout) != base+"+3" {
		t.Fatalf("third dispatch = %d in %q, want 0 in %s\n%s", third.code, jobLineOf(third.stdout), base+"+3", third.stderr)
	}
}

// A session held at dispatch refuses one fan-out member before it writes a
// record. Nothing can ever be collected for that member, so once the members
// that did run are delivered the name passes on — and while they are not, the
// refusal names the member that holds it rather than an "empty directory".
func TestARefusedFanMemberDoesNotPinTheName(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "sess-codex-held").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "sess-claude-free")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	if r := runEnvoyIn(t, e, project, "run", "pair", "--prompt-file", prompt, "--with", "codex", "--with", "claude:opus", "--timeout-min", "5"); r.code != 0 {
		t.Fatalf("round 1 = %d\n%s", r.code, r.stderr)
	}
	runEnvoyIn(t, e, project, "collect", "pair")

	lockDir := filepath.Join(e.home, ".local", "state", "envoy", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"pid":%d,"runnerInstanceId":"another-turn","outDir":"%s","startedAt":"2026-07-28T00:00:00.000Z"}`, os.Getpid(), t.TempDir())
	if err := os.WriteFile(filepath.Join(lockDir, "sess-codex-held.lock"), []byte(payload+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	round := func() runResult {
		return runEnvoyIn(t, e, project, "run", "pair-r2", "--prompt-file", prompt, "--with", "@pair", "--timeout-min", "5")
	}
	first := round()
	if first.code != 6 {
		t.Fatalf("round with one held member = %d, want 6\n%s", first.code, first.stderr)
	}
	r2 := jobLineOf(first.stdout)

	held := round()
	if held.code != 3 {
		t.Fatalf("dispatch under the uncollected round's name = %d, want 3\n%s", held.code, held.stderr)
	}
	mustContain(t, "stderr", held.stderr, "whose member claude-opus finished but has not been collected", "envoy collect '"+r2+"'")
	mustNotContain(t, "stderr", held.stderr, "empty directory", "holds no record")

	if r := runEnvoyIn(t, e, project, "collect", "pair-r2"); r.code != 0 {
		t.Fatalf("collect of the partial round = %d\n%s", r.code, r.stderr)
	}
	again := round()
	if again.code != 6 || jobLineOf(again.stdout) != r2+"+2" {
		t.Fatalf("after delivery the name must pass on: exit %d in %q, want 6 in %s\n%s", again.code, jobLineOf(again.stdout), r2+"+2", again.stderr)
	}
}

// A store that exists but cannot be listed is not an empty one: resolving a
// name there could serve an older generation as the latest, so collect and
// dispatch both stop instead.
func TestAnUnlistableStoreNeverServesAnOlderGeneration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	var store string
	for range 2 {
		r := runEnvoyIn(t, e, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
		if r.code != 0 {
			t.Fatalf("dispatch = %d\n%s", r.code, r.stderr)
		}
		store = filepath.Dir(jobLineOf(r.stdout))
		runEnvoyIn(t, e, project, "collect", "consult-r1")
	}
	// Traversable, not listable: generation 1 still opens by its bare path.
	if err := os.Chmod(store, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(store, 0o755) })

	col := runEnvoyIn(t, e, project, "collect", "--status-only", "consult-r1")
	if col.code != 2 {
		t.Fatalf("collect over an unlistable store = %d, want 2\nstdout:\n%s\nstderr:\n%s", col.code, col.stdout, col.stderr)
	}
	mustContain(t, "stderr", col.stderr, "could not be read", "not the same as an empty store")
	mustNotContain(t, "stdout", col.stdout, "status: ok")

	run := runEnvoyIn(t, e, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	if run.code != 2 {
		t.Fatalf("dispatch over an unlistable store = %d, want 2\n%s", run.code, run.stderr)
	}
	cont := runEnvoyIn(t, e, project, "run", "consult-r2", "--prompt-file", prompt, "--with", "@consult-r1", "--timeout-min", "5")
	if cont.code == 0 {
		t.Fatalf("a continuation must not resolve a name in an unlistable store\n%s", cont.stdout)
	}
	mustContain(t, "stderr", cont.stderr, "could not be read")
}

// A fan-out member with no record has not thereby been refused: members start
// independently, and one may still be preparing its turn after a sibling has
// finished and been collected. While the fan-out's process is alive the name
// is held; a dispatch that took it would run a second job under a name whose
// first is still starting.
func TestALiveFanOutHoldsItsNameForAMemberStillStarting(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_CALLER", "session-a")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	fifo := filepath.Join(t.TempDir(), "late-prompt.md")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("no fifo here: %v", err)
	}
	// Held open for writing, so opening the prompt never blocks but reading
	// it does, until this end writes and closes.
	late, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			late.WriteString("the late prompt\n")
			late.Close()
		}
	}

	cmd := exec.Command(binPath, "run", "pair", "--with", "codex="+fifo, "--with", "claude="+prompt, "--timeout-min", "5")
	cmd.Env, cmd.Dir = e.build(), project
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { release(); cmd.Wait() }()

	var group string
	var r runResult
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("the unblocked member never finished; last status read:\n%s%s", r.stdout, r.stderr)
		}
		r = runEnvoyIn(t, e, project, "collect", "--status-only", "pair")
		group = ""
		for line := range strings.SplitSeq(r.stdout, "\n") {
			if after, ok := strings.CutPrefix(line, "fan-out: "); ok {
				group = after
			}
		}
		if group != "" {
			if data, err := os.ReadFile(filepath.Join(group, "claude", "meta.json")); err == nil && strings.Contains(string(data), `"status": "ok"`) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(group, "codex", "meta.json")); err == nil {
		t.Fatal("the blocked member wrote a record; the rig no longer holds it before its first one")
	}
	runEnvoyIn(t, e, project, "collect", "pair") // delivers and stamps the finished sibling

	held := runEnvoyIn(t, e, project, "run", "pair", "--prompt-file", prompt, "--with", "codex", "--with", "claude", "--timeout-min", "5")
	if held.code != 3 {
		t.Fatalf("dispatch under a live fan-out's name = %d, want 3\nstdout:\n%s\nstderr:\n%s", held.code, held.stdout, held.stderr)
	}
	mustContain(t, "stderr", held.stderr, "whose member codex has written no record", "may still be alive")

	release()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the fan-out must finish once its late member can read its prompt: %v", err)
	}
	if readMeta(t, filepath.Join(group, "codex"))["status"] != "ok" {
		t.Fatal("the late member must have run")
	}
}

// A bare name is an address in the invoking project's central store: the
// job lands under ~/.local/state/envoy, never inside the project tree, and
// collect resolves the same name from the same project with no path at all.
func TestNamedJobsLiveInTheCentralStore(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoyIn(t, e, project, "run", "consult", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	var outDir string
	for line := range strings.SplitSeq(res.stdout, "\n") {
		if after, ok := strings.CutPrefix(line, "job: "); ok {
			outDir = after
			break
		}
	}
	jobsRoot := filepath.Join(e.home, ".local", "state", "envoy", "jobs")
	if !strings.HasPrefix(outDir, jobsRoot+string(filepath.Separator)) || filepath.Base(outDir) != "consult" {
		t.Fatalf("job %q must be <central store>/<project>/consult under %q", outDir, jobsRoot)
	}
	entries, _ := os.ReadDir(project)
	if len(entries) != 0 {
		t.Fatalf("the project tree must stay untouched, found %v", entries)
	}

	// Collect by name, from the project; the store is derived the same way.
	collected := runEnvoyIn(t, e, project, "collect", "consult")
	if collected.code != 0 {
		t.Fatalf("collect = %d\n%s", collected.code, collected.stderr)
	}
	mustContain(t, "collect stdout", collected.stdout,
		"job: "+outDir, "status: ok", "fake provider result")
	// Flags may come before or after the name.
	if r := runEnvoyIn(t, e, project, "collect", "--status-only", "consult"); r.code != 0 || !strings.Contains(r.stdout, "status: ok") {
		t.Fatalf("collect --status-only <name> = %d\n%s%s", r.code, r.stdout, r.stderr)
	}

	// And pending resolves the same project store from cwd alone.
	pending := runEnvoyIn(t, e, project, "pending")
	mustContain(t, "pending stdout", pending.stdout, "pending jobs: 0")

	// A fan-out member is reachable by name/member, from the project.
	if r := runEnvoyIn(t, e, project, "run", "pair", "--prompt-file", prompt, "--with", "codex", "--with", "claude", "--timeout-min", "5"); r.code != 0 {
		t.Fatalf("fan exit = %d\n%s", r.code, r.stderr)
	}
	if r := runEnvoyIn(t, e, project, "collect", "pair/codex"); r.code != 0 || !strings.Contains(r.stdout, "job: "+filepath.Join(filepath.Dir(outDir), "pair", "codex")) {
		t.Fatalf("collect pair/codex = %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if r := runEnvoyIn(t, e, project, "run", "pair-r2", "--prompt-file", prompt, "--with", "@pair/codex", "--with", "claude", "--timeout-min", "5"); r.code != 0 {
		t.Fatalf("warm member by name: exit = %d\n%s", r.code, r.stderr)
	}

	// collect without a job names nothing: there is no "newest" under
	// caller-chosen names, and a guess could deliver somebody else's result.
	bare := runEnvoyIn(t, e, project, "collect")
	if bare.code != 3 {
		t.Fatalf("bare collect: exit = %d, want 3\nstderr:\n%s", bare.code, bare.stderr)
	}
	mustContain(t, "stderr", bare.stderr, "collect takes the job to print")
}

// For a caller with no identity — this rig, a Codex session, a terminal — a
// name means the newest generation of anyone's. Callers in one long-lived
// checkout reach for the same names, so once a job is delivered the next
// dispatch under its name runs beside it, never over it, and collect and
// @<job> follow the name there. (Callers with an identity: caller_test.go.) The observed failure this pins: a refused re-run of
// review-r1 went unseen and collect review-r1 served a nine-day-old review.
func TestANameAddressesItsLatestGeneration(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	run := func(name string, extra ...string) runResult {
		return runEnvoyIn(t, e, project, append([]string{"run", name, "--prompt-file", prompt, "--timeout-min", "5"}, extra...)...)
	}
	jobLine := func(stdout string) string {
		for line := range strings.SplitSeq(stdout, "\n") {
			if after, ok := strings.CutPrefix(line, "job: "); ok {
				return after
			}
		}
		return ""
	}

	first := run("review-r1", "--with", "codex")
	if first.code != 0 {
		t.Fatalf("first dispatch = %d\n%s", first.code, first.stderr)
	}
	firstDir := jobLine(first.stdout)
	secondDir := firstDir + "+2"

	// Finished but uncollected: some caller may still be about to collect by
	// this name, so the name is held, nothing runs, and nothing is reserved.
	held := run("review-r1", "--with", "codex")
	if held.code != 3 {
		t.Fatalf("dispatch under a held name = %d, want 3\n%s", held.code, held.stderr)
	}
	mustNotContain(t, "a held-name refusal's stdout", held.stdout, "job:")
	mustContain(t, "stderr", held.stderr, "nothing was dispatched", "has not been collected",
		"now reads an earlier job, not a new one", "review-r1-b", "yours or its caller is gone", "envoy collect '"+firstDir+"'")
	if _, err := os.Stat(secondDir); !os.IsNotExist(err) {
		t.Fatalf("a refused dispatch must reserve nothing, found %s", secondDir)
	}

	// Delivered, the name passes on: the same command now runs, beside the
	// first job, and the first job's directory is untouched.
	if r := runEnvoyIn(t, e, project, "collect", "review-r1"); r.code != 0 || jobLine(r.stdout) != firstDir {
		t.Fatalf("collect of the first generation = %d, job %q\n%s", r.code, jobLine(r.stdout), r.stderr)
	}
	second := run("review-r1", "--with", "codex")
	if second.code != 0 || jobLine(second.stdout) != secondDir {
		t.Fatalf("second dispatch = %d in %q, want 0 in %q\n%s", second.code, jobLine(second.stdout), secondDir, second.stderr)
	}
	if readMeta(t, firstDir)["collectedAt"] == nil {
		t.Fatal("the first generation's record must survive the second dispatch")
	}

	// The name now reads the new job — owed, never stamped by the old one's
	// collection — and the old job stays reachable by its path.
	byName := runEnvoyIn(t, e, project, "collect", "review-r1")
	if byName.code != 0 || jobLine(byName.stdout) != secondDir {
		t.Fatalf("collect by name = %d, job %q, want %q", byName.code, jobLine(byName.stdout), secondDir)
	}
	mustNotContain(t, "first collect of the new generation", byName.stdout, "collected:")
	if r := runEnvoyIn(t, e, project, "collect", firstDir); r.code != 0 || jobLine(r.stdout) != firstDir {
		t.Fatalf("collect by path = %d, job %q, want %q", r.code, jobLine(r.stdout), firstDir)
	}

	// A continuation by name continues the latest generation's conversation.
	if r := run("review-r2", "--with", "@review-r1"); r.code != 0 {
		t.Fatalf("continuation by name = %d\n%s", r.code, r.stderr)
	}
	if got := readMeta(t, filepath.Join(filepath.Dir(firstDir), "review-r2"))["resumedFrom"]; got != secondDir {
		t.Fatalf("resumedFrom = %v, want the latest generation %s", got, secondDir)
	}

	// A fan-out holds its name until every member is delivered, then passes
	// it on whole; members resolve under the latest generation.
	if r := run("pair", "--with", "codex", "--with", "claude"); r.code != 0 {
		t.Fatalf("fan-out = %d\n%s", r.code, r.stderr)
	}
	if r := run("pair", "--with", "codex", "--with", "claude"); r.code != 3 {
		t.Fatalf("fan-out under a held name = %d, want 3\n%s", r.code, r.stderr)
	}
	if r := runEnvoyIn(t, e, project, "collect", "pair"); r.code != 0 {
		t.Fatalf("collect pair = %d\n%s", r.code, r.stderr)
	}
	if r := run("pair", "--with", "codex", "--with", "claude"); r.code != 0 {
		t.Fatalf("fan-out under a delivered name = %d\n%s", r.code, r.stderr)
	}
	pairTwo := filepath.Join(filepath.Dir(firstDir), "pair+2")
	if r := runEnvoyIn(t, e, project, "collect", "pair/codex"); r.code != 0 || jobLine(r.stdout) != filepath.Join(pairTwo, "codex") {
		t.Fatalf("collect pair/codex = %d, job %q, want it under %s", r.code, jobLine(r.stdout), pairTwo)
	}

	// A path is an identity and never passes on.
	outDir := filepath.Join(t.TempDir(), "job")
	if r := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("path job = %d\n%s", r.code, r.stderr)
	}
	runEnvoy(t, e, "collect", outDir)
	again := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...)
	if again.code != 3 {
		t.Fatalf("re-running a taken path = %d, want 3\n%s", again.code, again.stderr)
	}
	mustContain(t, "stderr", again.stderr, "a directory holds one job", "envoy collect '"+outDir+"'")
}

// A running job holds its name: its caller will collect by it when the
// process exits, and a dispatch that took the name would hand that caller
// another job's result.
func TestARunningJobHoldsItsName(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())

	cmd := exec.Command(binPath, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	cmd.Env = e.build()
	cmd.Dir = project
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}()

	// Contend only once the first job is visibly running under the name.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the first job never reported running")
		}
		if r := runEnvoyIn(t, e, project, "collect", "--status-only", "consult-r1"); strings.Contains(r.stdout, "status: running") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	held := runEnvoyIn(t, e, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	if held.code != 3 {
		t.Fatalf("dispatch under a running job's name = %d, want 3\nstdout:\n%s\nstderr:\n%s", held.code, held.stdout, held.stderr)
	}
	mustContain(t, "stderr", held.stderr, "is still running")
	mustContain(t, "stderr", held.stderr, "nothing was dispatched", "its own caller has collected it")
}

// An engine whose meta schema moved must not take the store's names hostage.
// A generation an earlier engine wrote still says whose it is, so a reused
// name resolves and dispatches as before; the earlier job itself is refused
// by name when read, never reinterpreted — and since this engine cannot
// deliver it, it holds no name, even uncollected.
func TestAnEarlierSchemasJobsLeaveTheirNamesWorking(t *testing.T) {
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	a := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").as("session-a", "")
	probe := runEnvoyIn(t, a, project, "run", "probe", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	if probe.code != 0 {
		t.Fatalf("probe dispatch = %d\n%s", probe.code, probe.stderr)
	}
	store := filepath.Dir(jobLineOf(probe.stdout))
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	earlier := map[string]string{
		"consult-r1": `{"schemaVersion":9,"status":"ok","provider":"codex","caller":"old-session","collectedAt":"2026-09-01T00:00:00.000Z"}`,
		"review-r1":  `{"schemaVersion":9,"status":"ok","provider":"codex","collectedAt":null}`,
		"live-r1":    fmt.Sprintf(`{"schemaVersion":9,"status":"running","provider":"codex","caller":"session-a","runnerPid":%d}`, os.Getpid()),
		"dead-r1":    fmt.Sprintf(`{"schemaVersion":9,"status":"running","provider":"codex","caller":"session-a","runnerPid":%d}`, gone.Process.Pid),
		"locked-r1":  `{"schemaVersion":9,"status":"infra","provider":"codex","sessionLockConflict":"session s has a live turn; run envoy collect '/tmp/x'","collectedAt":null}`,
	}
	for name, meta := range earlier {
		dir := filepath.Join(store, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := runEnvoyIn(t, a, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	if r.code != 0 || jobLineOf(r.stdout) != filepath.Join(store, "consult-r1+2") {
		t.Fatalf("reusing a name over an earlier engine's job = %d, job %q\n%s", r.code, jobLineOf(r.stdout), r.stderr)
	}
	if col := runEnvoyIn(t, a, project, "collect", "--status-only", "consult-r1"); col.code != 0 || jobLineOf(col.stdout) != filepath.Join(store, "consult-r1+2") {
		t.Fatalf("the name must mean the caller's own new job: %d\n%s%s", col.code, col.stdout, col.stderr)
	}
	old := runEnvoyIn(t, a, project, "collect", filepath.Join(store, "consult-r1"))
	if old.code != 3 {
		t.Fatalf("an earlier schema's job must be refused when read, got %d\n%s", old.code, old.stdout)
	}
	mustContain(t, "stderr", old.stderr, "schema 9")
	// The version is read before anything it changed: a field another schema
	// gave a different shape is refused as that schema, never as a decode error.
	locked := runEnvoyIn(t, a, project, "collect", filepath.Join(store, "locked-r1"))
	if locked.code != 3 {
		t.Fatalf("an earlier schema's lock refusal must be refused when read, got %d\n%s", locked.code, locked.stdout)
	}
	mustContain(t, "stderr", locked.stderr, "schema 9")
	mustNotContain(t, "stderr", locked.stderr, "unmarshal")

	if r := runEnvoyIn(t, a.as("", ""), project, "run", "review-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5"); r.code != 0 {
		t.Fatalf("an earlier schema's uncollected job must not hold its name: %d\n%s", r.code, r.stderr)
	}
	// A turn the earlier engine is still running is live work under the name:
	// it holds for as long as its runner may be alive, and only then lets go.
	if r := runEnvoyIn(t, a, project, "run", "live-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5"); r.code != 3 {
		t.Fatalf("an earlier engine's live turn must hold its name: %d\n%s", r.code, r.stdout)
	}
	if r := runEnvoyIn(t, a, project, "run", "dead-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5"); r.code != 0 {
		t.Fatalf("an earlier engine's turn whose runner is gone must not hold its name: %d\n%s", r.code, r.stderr)
	}

	// The recovery index reads the same stamp: a collected earlier job owes
	// nothing, a live one is skipped like any live job, and an uncollected one
	// is listed for what it is — intact, and another version's — never as damage.
	pend := runEnvoyIn(t, a, project, "pending")
	mustContain(t, "pending", pend.stdout, "[other-schema] "+filepath.Join(store, "review-r1")+"\n", "schema 9",
		"[other-schema] "+filepath.Join(store, "dead-r1")+"\n", "[other-schema] "+filepath.Join(store, "locked-r1")+"\n")
	mustNotContain(t, "pending", pend.stdout, filepath.Join(store, "consult-r1")+"\n", filepath.Join(store, "live-r1")+"\n", "corrupt", "damaged")
}

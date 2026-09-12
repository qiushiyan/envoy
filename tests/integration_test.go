// Package integration exercises the built envoy binary end to end against
// the fake provider in fake-bin/, without billing a model. The suite covers
// the lifecycle contract: ok paths, failure recovery, timeouts, interruption,
// the terminal-envelope race, lock safety, and collection.
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	binPath string
	fakeBin string
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "envoy-test-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)
	binPath = filepath.Join(tmp, "envoy")
	// -race here is what actually exercises the runner's goroutines under the
	// race detector: they live in this subprocess, not in the test process.
	build := exec.Command("go", "build", "-race", "-o", binPath, "./cmd/envoy")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	wd, _ := os.Getwd()
	fakeBin = filepath.Join(wd, "fake-bin")
	os.Exit(m.Run())
}

type env struct {
	home  string
	extra map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{home: t.TempDir(), extra: map[string]string{}}
}

func (e *env) set(k, v string) *env {
	e.extra[k] = v
	return e
}

func (e *env) build() []string {
	out := []string{
		"HOME=" + e.home,
		"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
		// The race runtime sleeps a full second before exiting by default, to
		// give still-running threads a chance to trip the detector on the way
		// out. Every test here spawns at least one envoy, so that second was
		// most of the suite's wall time — ~105s down to ~17s without it. It
		// costs nothing this suite was buying: detection during the run is
		// unaffected, and the runner tears its own goroutines down before
		// exit, which the lifecycle tests assert on directly.
		"GORACE=atexit_sleep_ms=0",
		"ENVOY_HEARTBEAT_MS=100",
		"ENVOY_TIMEOUT_POLL_MS=25",
		"ENVOY_SIGKILL_AFTER_MS=400",
		"ENVOY_CLOSE_GRACE_MS=150",
	}
	for k, v := range e.extra {
		out = append(out, k+"="+v)
	}
	return out
}

type runResult struct {
	code   int
	stdout string
	stderr string
}

func runEnvoy(t *testing.T, e *env, args ...string) runResult {
	t.Helper()
	return runEnvoyIn(t, e, "", args...)
}

func runEnvoyIn(t *testing.T, e *env, dir string, args ...string) runResult {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	cmd.Env = e.build()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running envoy: %v", err)
		}
	}
	return runResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func writePrompt(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "prompt-src.md")
	if err := os.WriteFile(p, []byte("fake prompt body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readMeta(t *testing.T, outDir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outDir, "meta.json"))
	if err != nil {
		t.Fatalf("meta.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("meta.json parse: %v\n%s", err, data)
	}
	return m
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(data)
}

func mustContain(t *testing.T, name, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("%s must contain %q, got:\n%s", name, sub, s)
		}
	}
}

func mustNotContain(t *testing.T, name, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Fatalf("%s must not contain %q, got:\n%s", name, sub, s)
		}
	}
}

// runArgs is the dispatch form every test uses: the job named up front, the
// prompt, then whatever voices and flags the case adds.
func runArgs(prompt, outDir string, extra ...string) []string {
	args := []string{"run", outDir, "--prompt-file", prompt}
	return append(args, extra...)
}

func TestCodexSuccess(t *testing.T) {
	receivedPrompt := filepath.Join(t.TempDir(), "received-prompt.txt")
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_PROMPT_FILE", receivedPrompt)
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"job: "+outDir,
		"provider: codex · model (provider default) · effort (provider default) · hard cap 5m",
		"next: let this command run to completion, then collect the job: envoy collect",
		"status: ok",
		"session: fake-session-id",
		"next: Collect and verify this job: envoy collect",
	)
	// The caller is an agent that names the job and collects by that name:
	// nothing in the dispatch block is a coordinate it must read back, and
	// nothing addresses a human at a keyboard.
	mustNotContain(t, "stdout", res.stdout, "takeover", "watch:", "out-dir:")
	if got := readFile(t, filepath.Join(outDir, "result.md")); got != "fake provider result" {
		t.Fatalf("result.md = %q", got)
	}
	if got := readFile(t, filepath.Join(outDir, "prompt.md")); got != "fake prompt body\n" {
		t.Fatalf("prompt.md = %q", got)
	}
	// prompt.md only proves the file copy; the provider must have received the
	// same bytes on stdin — a truncated or early-closed write would otherwise
	// dispatch an empty prompt while every other assertion stays green.
	if got := readFile(t, receivedPrompt); got != "fake prompt body\n" {
		t.Fatalf("provider received prompt %q on stdin, want the dispatched bytes", got)
	}

	meta := readMeta(t, outDir)
	if meta["status"] != "ok" || meta["promptState"] != "accepted" {
		t.Fatalf("meta = status %v prompt %v", meta["status"], meta["promptState"])
	}
	if meta["promptStateEvidence"] != "codex thread.started" {
		t.Fatalf("evidence = %v", meta["promptStateEvidence"])
	}
	// The recorded follow-up is a complete command, not a fragment the caller
	// has to assemble — it names this job, whose records carry the session and
	// the settings, and leaves open only what the caller must choose.
	resume, _ := meta["resumeCommand"].(string)
	for _, want := range []string{"envoy run <new-job-name>", "--with @'" + outDir + "'", "--timeout-min 5", "--prompt-file <your-follow-up.md>"} {
		if !strings.Contains(resume, want) {
			t.Fatalf("resumeCommand %q is missing %q", resume, want)
		}
	}
	tokens := meta["tokens"].(map[string]any)
	if tokens["input"] != float64(13) || tokens["cachedInput"] != float64(5) || tokens["reasoningOutput"] != float64(3) {
		t.Fatalf("tokens = %v", tokens)
	}
	if v, ok := meta["collectedAt"]; !ok || v != nil {
		t.Fatalf("collectedAt must be explicit null before collection, got %v (present=%v)", v, ok)
	}
	if meta["providerReportedModel"] != nil {
		t.Fatalf("codex reports no model; the field must stay null, got %v", meta["providerReportedModel"])
	}

	progress := readFile(t, filepath.Join(outDir, "progress.log"))
	mustContain(t, "progress.log", progress, "state=starting", "state=accepted", "state=provider-terminal", "state=terminal status=ok")
	if raw := readFile(t, filepath.Join(outDir, "raw.log")); !strings.Contains(raw, `"thread.started"`) {
		t.Fatalf("raw.log must keep verbatim provider stdout:\n%s", raw)
	}
	locks, _ := os.ReadDir(filepath.Join(e.home, ".local", "state", "envoy", "locks"))
	if len(locks) != 0 {
		t.Fatalf("session lock must be released, found %d", len(locks))
	}
}

func TestClaudeSuccessWithUnterminatedFinalLine(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude", "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	// The fake writes the final result envelope without a trailing newline;
	// the close-time flush must still parse it.
	if got := readFile(t, filepath.Join(outDir, "result.md")); got != "fake provider result" {
		t.Fatalf("result.md = %q", got)
	}
	meta := readMeta(t, outDir)
	if meta["sessionId"] != "fake-session-id" {
		t.Fatalf("the result envelope's session id must win, got %v", meta["sessionId"])
	}
	if meta["costUsd"] != float64(0.01) {
		t.Fatalf("cost = %v", meta["costUsd"])
	}
	tokens := meta["tokens"].(map[string]any)
	if tokens["cacheRead"] != float64(4) || tokens["cacheCreation"] != float64(2) {
		t.Fatalf("tokens = %v", tokens)
	}
	mustContain(t, "terminal stdout", res.stdout, "session: fake-session-id")

	// The provider announced its resolved model; the engine records the
	// observation without inferring anything from the (absent) request.
	if meta["providerReportedModel"] != "fake-claude-model" {
		t.Fatalf("providerReportedModel = %v", meta["providerReportedModel"])
	}
	mustContain(t, "progress.log", readFile(t, filepath.Join(outDir, "progress.log")),
		"state=provider-initialized session=fake-session-id model=fake-claude-model")
	// A default that simply resolved is not a substitution: the caller asked
	// for the provider's own choice and got it, so an ok block says nothing
	// about it. The observation is still there for anyone who asks.
	collected := runEnvoy(t, e, "collect", outDir)
	if strings.Contains(collected.stdout, "provider: claude") {
		t.Fatalf("an ok block must not restate the settings the caller passed:\n%s", collected.stdout)
	}
	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	mustContain(t, "status-only stdout", status.stdout,
		"provider: claude · model (provider default, ran fake-claude-model) · effort (provider default)")
}

// A reported model that differs from the requested one is not evidence of a
// substitution: `opus` against `claude-opus-5` is the provider's own alias
// resolution, the mapping envoy refuses to own because it is the provider's to
// change. String inequality cannot tell that apart from a real substitution, so
// the block does not try — treating the difference as a surprise would be
// inferring, and would fire on the most ordinary claude dispatch there is. The
// observation stays in meta.json and --status-only, where it is provable.
func TestCollectOkBlockDoesNotGuessAtModelSubstitution(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude:opus",
		"--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if got := readMeta(t, outDir)["providerReportedModel"]; got != "fake-claude-model" {
		t.Fatalf("the reported model must still be recorded, got %v", got)
	}

	// The resume command legitimately carries `--model opus` — it replays the
	// dispatch — so the assertion is on the settings line itself.
	collected := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "collect stdout", collected.stdout, "status: ok")
	mustNotContain(t, "ok block", collected.stdout,
		"provider: claude · model", "fake-claude-model", "label job")

	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	mustContain(t, "status-only stdout", status.stdout,
		"provider: claude · model opus (ran fake-claude-model) · effort (provider default) · label job")
}

func TestClaudePartialFailure(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "partial-failure")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude", "--timeout-min", "5")...)
	if res.code != 1 {
		t.Fatalf("exit = %d, want 1\nstderr:\n%s", res.code, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "failed" || meta["resultKind"] != "partial" {
		t.Fatalf("meta = status %v kind %v", meta["status"], meta["resultKind"])
	}
	if meta["childExitCode"] != float64(7) {
		t.Fatalf("childExitCode = %v", meta["childExitCode"])
	}
	result := readFile(t, filepath.Join(outDir, "result.md"))
	mustContain(t, "result.md", result,
		"# Turn failed",
		"synthetic provider failure",
		"## Partial output recovered before the failure",
		"useful claude work before failure",
	)
	if strings.Count(result, "synthetic provider failure") != 1 {
		t.Fatalf("the error echo must be excluded from the partial:\n%s", result)
	}
}

func TestCodexTimeoutRecordsAcceptance(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "0.02")...)
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "timeout" || meta["promptState"] != "accepted" {
		t.Fatalf("meta = status %v prompt %v", meta["status"], meta["promptState"])
	}
	mustContain(t, "error", meta["error"].(string),
		"wall-clock cap ended this codex turn",
		"not evidence the provider hung")
	// This scenario is the shape two real timeouts took: the provider announced
	// its thread and then streamed nothing until the cap. The envelope must
	// carry that observation, because the caller cannot otherwise tell it from
	// a turn that worked right up to the deadline — and the follow-up each one
	// deserves is different.
	mustContain(t, "error", meta["error"].(string),
		"the stream had been quiet for", "after 1 event (last: thread.started)")
	mustContain(t, "recoveryAction", meta["recoveryAction"].(string),
		"--with @'"+outDir+"'", "--timeout-min 0.02",
		"would repeat work that already happened")
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")), "# Turn timeout")
}

// "When the cap arrived" has to mean the cap, not the end of cleanup. A
// provider that streams one more event while being torn down would otherwise
// be reported as busier and more recently active than it was at the deadline —
// the engine describing its own teardown back to the caller as provider work.
func TestCodexTimeoutReportsTheStreamAsOfTheCap(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "quiet-then-emits-on-term")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "0.02")...)
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	mustContain(t, "error", meta["error"].(string), "after 1 event (last: thread.started)")
	// The teardown event still belongs in the record — it happened — it just
	// may not be dressed up as the state the cap found.
	if got := meta["providerEventCount"].(float64); got != 2 {
		t.Fatalf("the drained teardown event must still be counted: providerEventCount = %v", got)
	}
	if got := meta["lastProviderEventType"]; got != "item.updated" {
		t.Fatalf("meta must record the last event actually seen: %v", got)
	}
}

// A cap that fires before the provider says anything at all has no quiet
// interval to report, only the absence of a stream. The envelope says that
// instead of inventing a duration — and it reports the absence, without
// concluding from it that no work happened anywhere.
func TestCodexTimeoutBeforeAnyOutputReportsAnEmptyStream(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "0.02")...)
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	mustContain(t, "error", meta["error"].(string),
		"wall-clock cap ended this codex turn",
		"The provider wrote nothing at all before the cap — no output on either stream, and no events.")
	if strings.Contains(meta["error"].(string), "quiet for") {
		t.Fatalf("no output means no quiet interval to report: %v", meta["error"])
	}
}

// Bytes on the wire are not events envoy can read, and the envelope must not
// collapse the two: a provider that writes a diagnostic and then stalls did
// stream something, and reporting "nothing at all" there is false — the shape
// a real stalled codex turn takes on a machine with a corrupt models cache.
func TestCodexTimeoutSeparatesBytesFromEvents(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "stderr-noise-then-silence")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "0.02")...)
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	errText := readMeta(t, outDir)["error"].(string)
	mustContain(t, "error", errText, "bytes before the cap but no event envoy could parse")
	for _, forbidden := range []string{"nothing at all", "no work of its own"} {
		if strings.Contains(errText, forbidden) {
			t.Fatalf("output did arrive, so %q is false here: %s", forbidden, errText)
		}
	}
}

func TestCodexTerminalEnvelopeWinsDuringCleanup(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "terminal-success-then-hang")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "0.02")...)
	if res.code != 0 {
		t.Fatalf("an observed turn.completed must beat the cap: exit = %d\nstdout:\n%s\nstderr:\n%s",
			res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "ok" {
		t.Fatalf("status = %v", meta["status"])
	}
	if got := readFile(t, filepath.Join(outDir, "result.md")); got != "fake provider result" {
		t.Fatalf("result.md = %q", got)
	}
	if meta["providerTerminalEventType"] != "codex turn.completed" {
		t.Fatalf("terminal event = %v", meta["providerTerminalEventType"])
	}
}

func TestClaudeStubbornGrandchildTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "hang-with-stubborn-grandchild-only").
		set("ENVOY_FAKE_GRANDCHILD_PID_FILE", pidFile)
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	start := time.Now()
	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude", "--timeout-min", "0.02")...)
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("a SIGTERM-ignoring grandchild holding the pipes must not stall the runner (took %s)", elapsed)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "timeout" || meta["promptState"] != "accepted" {
		t.Fatalf("meta = status %v prompt %v", meta["status"], meta["promptState"])
	}
	// The whole process tree must be gone, grandchild included.
	var pid int
	fmt.Sscanf(readFile(t, pidFile), "%d", &pid)
	if pid > 0 {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("grandchild %d must be dead after cleanup", pid)
	}
}

func TestClaudeTranscriptRecoveryOnTimeout(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "transcript-only-hang")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude", "--timeout-min", "0.02")...)
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["promptState"] != "accepted" || meta["promptStateEvidence"] != "claude session transcript" {
		t.Fatalf("transcript evidence must prove acceptance: prompt %v evidence %v",
			meta["promptState"], meta["promptStateEvidence"])
	}
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")),
		"partial work from the Claude transcript")
}

// Continuing a job whose session another live turn holds is refused before
// anything is written — and the name the refused run was given goes back
// into circulation, since nothing ran under it.
func TestResumeLockConflictFailsFast(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())
	first := filepath.Join(t.TempDir(), "consult")
	if r := runEnvoy(t, e, runArgs(prompt, first, "--with", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("first turn exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	lockDir := filepath.Join(e.home, ".local", "state", "envoy", "locks")
	os.MkdirAll(lockDir, 0o755)
	payload := fmt.Sprintf(`{"pid":%d,"runnerInstanceId":"other","outDir":"/tmp/live-job","startedAt":"2026-07-25T00:00:00.000Z"}`, os.Getpid())
	os.WriteFile(filepath.Join(lockDir, "fake-session-id.lock"), []byte(payload), 0o644)

	outDir := filepath.Join(t.TempDir(), "job")
	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "@"+first)...)
	if res.code != 3 {
		t.Fatalf("exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "lock error:", "already has a live turn", "/tmp/live-job")
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Fatal("a refused continuation must leave no job dir behind — the name is free again")
	}
}

func TestCodexFreshSessionLockCollision(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	lockDir := filepath.Join(e.home, ".local", "state", "envoy", "locks")
	os.MkdirAll(lockDir, 0o755)
	payload := fmt.Sprintf(`{"pid":%d,"runnerInstanceId":"other","outDir":"/tmp/live-job","startedAt":"2026-07-25T00:00:00.000Z"}`, os.Getpid())
	os.WriteFile(filepath.Join(lockDir, "fake-session-id.lock"), []byte(payload), 0o644)

	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...)
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "lock error:")
	meta := readMeta(t, outDir)
	if meta["sessionLockConflict"] == nil {
		t.Fatal("meta must record the lock conflict")
	}
	if meta["resumeCommand"] != nil {
		t.Fatalf("a conflicted session must suppress its resume coordinate: %v", meta["resumeCommand"])
	}
	if meta["promptStateEvidence"] != "codex thread.started with conflicting session lock" {
		t.Fatalf("evidence = %v", meta["promptStateEvidence"])
	}
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")),
		"# Turn infra", "another turn already holds")
}

func TestInterruptRecordsPartialAndResume(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	cmd := exec.Command(binPath, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...)
	cmd.Env = e.build()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Interrupt once the provider has provably accepted the prompt.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("provider never accepted; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
		if data, err := os.ReadFile(filepath.Join(outDir, "meta.json")); err == nil &&
			strings.Contains(string(data), `"promptState": "accepted"`) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if code != 5 {
		t.Fatalf("exit = %d, want 5\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "interrupted" || meta["interruptionSignal"] != "SIGINT" {
		t.Fatalf("meta = status %v signal %v", meta["status"], meta["interruptionSignal"])
	}
	mustContain(t, "error", meta["error"].(string), "stopped codex after receiving SIGINT")
	mustContain(t, "recoveryAction", meta["recoveryAction"].(string),
		"envoy run <new-job-name>", "--with @'"+outDir+"'", "--timeout-min 5")
}

func TestCollectStampsAndPendingDiscovers(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	base := t.TempDir()
	outDir := filepath.Join(base, "20260725-120000-consult")
	prompt := writePrompt(t, t.TempDir())

	if res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); res.code != 0 {
		t.Fatalf("turn failed: %d\n%s", res.code, res.stderr)
	}

	pending := runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending stdout", pending.stdout,
		"pending jobs: 1",
		"[terminal:ok] "+outDir,
		"terminal status ok has not been collected",
		"next: envoy collect")

	collected := runEnvoy(t, e, "collect", outDir)
	if collected.code != 0 {
		t.Fatalf("collect = %d\n%s", collected.code, collected.stderr)
	}
	mustContain(t, "collect stdout", collected.stdout,
		"job: "+outDir,
		"status: ok",
		"resume: envoy run <new-job-name> --with @'"+outDir+"' --timeout-min 5 --prompt-file <your-follow-up.md>",
		"--- result.md ---",
		"fake provider result",
		"next: result.md above is this turn's return value")

	meta := readMeta(t, outDir)
	if meta["collectedAt"] == nil {
		t.Fatal("collection must stamp collectedAt")
	}

	pending = runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending after collect", pending.stdout, "pending jobs: 0", "no recovery action is needed")
}

// The block is read by an agent whose context the result body competes for, so
// a turn that worked prints what the caller acts on and nothing else. Every
// field held back here answers a question only a turn that went wrong raises,
// and none of them is lost: meta.json is the record, and --status-only prints
// the whole preamble on demand.
func TestCollectOkBlockHoldsBackDiagnostics(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	if res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); res.code != 0 {
		t.Fatalf("turn failed: %d\n%s", res.code, res.stderr)
	}

	collected := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "ok block", collected.stdout,
		"job: "+outDir,
		"status: ok",
		"duration: ",
		"resume: envoy run <new-job-name> --with @'"+outDir+"'",
		"fake provider result")
	mustNotContain(t, "ok block", collected.stdout,
		"provider: codex",
		"tokens: ",
		"prompt: accepted",
		"result kind: ",
		"logs: progress ",
		// The continuation names the job, not the session; a bare id line
		// is one more chance to hand-assemble a follow-up out of it.
		"session: fake-session-id\n",
		"takeover")

	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	mustContain(t, "status-only block", status.stdout,
		"status: ok",
		"provider: codex · model (provider default) · effort (provider default)",
		"tokens: input 13 · cachedInput 5 · output 8 · reasoningOutput 3",
		"prompt: accepted",
		"result kind: final",
		"logs: progress ",
		"next: this was a status check only")
	// Asking for the whole preamble asks for every field, not for the same
	// identifier three times: the bare line stays suppressed wherever a
	// command below already spells the session out.
	mustNotContain(t, "status-only block", status.stdout, "session: fake-session-id\n")
}

// The mirror: a turn that did not return a result keeps every diagnostic field,
// because its recovery prose reasons from them. The log paths in particular
// must survive — the next line names those three files by name, and a
// prescription may not point at paths the block withheld.
func TestCollectFailedBlockCarriesDiagnostics(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "nonzero-final")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	if res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); res.code != 1 {
		t.Fatalf("turn exit = %d, want 1\n%s", res.code, res.stderr)
	}

	collected := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "failed block", collected.stdout,
		"status: failed — the provider ran and reported a failure",
		"provider: codex",
		"tokens: ",
		"prompt: accepted",
		"result kind: partial",
		"logs: progress "+filepath.Join(outDir, "progress.log"),
		"raw "+filepath.Join(outDir, "raw.log"),
		"stderr "+filepath.Join(outDir, "stderr.log"),
		"resume: envoy run <new-job-name> --with @'"+outDir+"'",
		"next: The provider accepted this prompt")
	// Even here the id is not repeated: the resume command carries it.
	mustNotContain(t, "failed block", collected.stdout, "session: fake-session-id\n")
}

// A store that could not be read is not an empty one. Reporting a mistyped
// --base as "no turn has run" — or, worse, as "no recovery action is needed" —
// states as fact something the engine never observed. A base that simply does
// not exist yet is the opposite case: that is exactly how a project looks
// before its first dispatch, and it must stay quiet and successful.
func TestPendingSeparatesAnUnreadableStoreFromAnEmptyOne(t *testing.T) {
	e := newEnv(t)

	// A base the caller *named* and got wrong raises the same "not found" as a
	// store awaiting its first dispatch, and the two mean opposite things. Only
	// the derived default may be waved through: a typo must refuse, or the
	// listing answers a question about a project the caller never asked about.
	absent := filepath.Join(t.TempDir(), "never-dispatched")
	for _, cmd := range []string{"pending"} {
		res := runEnvoy(t, e, cmd, "--base", absent)
		if res.code != 2 {
			t.Fatalf("%s on an explicit --base that does not exist must refuse, got exit %d\nstdout:\n%s", cmd, res.code, res.stdout)
		}
		mustContain(t, cmd+" stderr", res.stderr, "could not be read", "not the same as an empty store")
	}

	// The derived default store is created by the first dispatch, so its
	// absence is exactly how a project looks before it has ever run a turn.
	project := t.TempDir()
	for _, cmd := range []string{"pending"} {
		res := runEnvoyIn(t, e, project, cmd)
		if res.code != 0 {
			t.Fatalf("%s in a project that never dispatched is a normal first run: exit %d\n%s", cmd, res.code, res.stderr)
		}
	}

	sealed := filepath.Join(t.TempDir(), "sealed")
	if err := os.Mkdir(sealed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sealed, 0o755) })
	for _, cmd := range []string{"pending"} {
		res := runEnvoy(t, e, cmd, "--base", sealed)
		if res.code != 2 {
			t.Fatalf("%s over an unreadable store must fail as infra, got exit %d\nstdout:\n%s", cmd, res.code, res.stdout)
		}
		mustContain(t, cmd+" stderr", res.stderr, "could not be read", "not the same as an empty store")
		for _, forbidden := range []string{"no recovery action is needed"} {
			if strings.Contains(res.stdout, forbidden) {
				t.Fatalf("%s must not claim %q over a store it could not read:\n%s", cmd, forbidden, res.stdout)
			}
		}
	}
}

func TestCollectReconcilesAbandonedJob(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	outDir := filepath.Join(base, "20260725-130000-delegate")
	os.MkdirAll(outDir, 0o755)
	os.WriteFile(filepath.Join(outDir, "prompt.md"), []byte("x"), 0o644)
	meta := map[string]any{
		"schemaVersion": 4, "status": "running", "provider": "codex",
		"promptState": "accepted", "sessionId": "dead-session",
		"resumeCommand": "envoy turn --provider codex --resume dead-session --timeout-min 180 --prompt-file <your-follow-up.md>",
		"runnerPid":     4194304, "providerPid": 4194304, "providerPgid": 4194304,
		"timeoutMin": 180.0, "nextAction": "wait", "resultKind": "none",
		"collectedAt": nil,
	}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(outDir, "meta.json"), data, 0o644)

	res := runEnvoy(t, e, "collect", outDir)
	if res.code != 0 {
		t.Fatalf("collect = %d\n%s", res.code, res.stderr)
	}
	mustContain(t, "collect stdout", res.stdout,
		"status: abandoned — the process ended without publishing a result",
		"This turn ended without publishing a result",
		"continue the same session with a follow-up prompt",
		"envoy run <new-job-name> --with @'"+outDir+"'")
	// The persisted command names a verb that no longer exists; it is never
	// replayed, the records are.
	mustNotContain(t, "collect stdout", res.stdout, "envoy turn", "--resume dead-session")
	got := readMeta(t, outDir)
	if got["status"] != "abandoned" || got["reconciledAt"] == nil {
		t.Fatalf("reconciled meta = status %v reconciledAt %v", got["status"], got["reconciledAt"])
	}
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")), "# Turn abandoned")
}

func TestExitBeforeStdinDoesNotCrash(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "exit-before-stdin")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...)
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "infra" || meta["childExitCode"] != float64(23) {
		t.Fatalf("meta = status %v exit %v", meta["status"], meta["childExitCode"])
	}
	mustContain(t, "error", meta["error"].(string), "fake provider exited before reading its prompt")
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
	if got := readMeta(t, outDir)["label"]; got != "consult" {
		t.Fatalf("label = %v, want the job's name", got)
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

	// A name is used once.
	again := runEnvoyIn(t, e, project, "run", "consult", "--prompt-file", prompt, "--with", "codex")
	if again.code != 3 {
		t.Fatalf("re-running a taken name: exit = %d, want 3\nstderr:\n%s", again.code, again.stderr)
	}
	mustContain(t, "stderr", again.stderr, "already exists", "envoy collect '"+outDir+"'")

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

// The r.term == nil residual-cleanup chain: the provider exits cleanly while
// a SIGTERM-ignoring grandchild keeps the pipes open. This chain — exit
// fallback → group probe → TERM → KILL → finalize — is the entire reason the
// runner passes raw pipe fds instead of exec's managed pipes; if the fallback
// timer were dropped, this test would hang instead of finishing ok.
func TestStubbornGrandchildAfterSuccessIsReaped(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	readyFile := filepath.Join(t.TempDir(), "grandchild.ready")
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success-with-stubborn-grandchild").
		set("ENVOY_FAKE_GRANDCHILD_PID_FILE", pidFile).
		set("ENVOY_FAKE_GRANDCHILD_READY_FILE", readyFile)
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "ok" {
		t.Fatalf("status = %v", meta["status"])
	}
	if got := readFile(t, filepath.Join(outDir, "result.md")); got != "fake provider result" {
		t.Fatalf("result.md = %q", got)
	}
	var pid int
	fmt.Sscanf(readFile(t, pidFile), "%d", &pid)
	if pid <= 0 {
		t.Fatalf("grandchild pid file unreadable: %q", readFile(t, pidFile))
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d must be dead after residual cleanup", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Split multi-byte UTF-8 and a result envelope split across writes must
// reassemble; guards the byte-level line buffer against a rewrite onto a
// line scanner that decodes or caps mid-chunk.
func TestClaudeFragmentedStreamAssembles(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "fragmented-success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude", "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	if got := readFile(t, filepath.Join(outDir, "result.md")); got != "fake provider result" {
		t.Fatalf("result.md = %q", got)
	}
	if raw := readFile(t, filepath.Join(outDir, "raw.log")); !strings.Contains(raw, "🧭") {
		t.Fatal("raw.log must carry the split multi-byte rune verbatim")
	}
}

// A codex CLI that produced a full response but exited non-zero is a failed
// turn with a recovered partial — not ok (the exit code is real) and not
// infra (the response exists).
func TestCodexNonzeroExitAfterResponse(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "nonzero-final")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...)
	if res.code != 1 {
		t.Fatalf("exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "failed" || meta["resultKind"] != "partial" || meta["promptState"] != "accepted" {
		t.Fatalf("meta = status %v kind %v prompt %v", meta["status"], meta["resultKind"], meta["promptState"])
	}
	mustContain(t, "error", meta["error"].(string), "exited with code 7 after producing a response")
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")), "fake provider result")
}

// system/init proves only that the process launched — it must never count as
// prompt acceptance, or a turn that died before model work would be steered
// to "resume, never redispatch" for work that never began.
func TestClaudeInitOnlyIsNotAcceptance(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "init-only-hang")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude", "--timeout-min", "0.02")...)
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["promptState"] != "unknown" {
		t.Fatalf("init alone must not prove acceptance, got %v (evidence %v)",
			meta["promptState"], meta["promptStateEvidence"])
	}
	mustContain(t, "recoveryAction", meta["recoveryAction"].(string),
		"silence is not proof", "only once they show it never began")
}

// The fan-out contract: one command, one completion, one directory — and
// underneath it, members that are ordinary turns in every respect.
func TestFanDispatchesEveryMemberAsAnOrdinaryTurn(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "group")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"job: "+outDir,
		"fan-out: 2 turns · one prompt · hard cap 5m each",
		"member codex: model (provider default) · effort (provider default) · dir "+filepath.Join(outDir, "codex"),
		"member claude-opus: model opus · effort (provider default) · dir "+filepath.Join(outDir, "claude-opus"),
		"next: let this command run to completion — it exits once every member is done",
		"status: ok — all 2 turns returned a result",
		"member codex: ok · result ",
		"member claude-opus: ok · result ",
		"next: Collect the fan-out: envoy collect ",
	)
	// A member's stdout block would interleave with its siblings', so the
	// single-turn lines stay in each member's own meta.json.
	if strings.Contains(res.stdout, "next: Collect and verify this job") {
		t.Fatalf("member turn blocks must not reach the fan-out's stdout:\n%s", res.stdout)
	}

	for name, wantProvider := range map[string]string{"codex": "codex", "claude-opus": "claude"} {
		memberDir := filepath.Join(outDir, name)
		meta := readMeta(t, memberDir)
		if meta["status"] != "ok" || meta["provider"] != wantProvider || meta["promptState"] != "accepted" {
			t.Fatalf("%s meta = status %v provider %v prompt %v", name, meta["status"], meta["provider"], meta["promptState"])
		}
		if meta["allowWrite"] != false {
			t.Fatalf("%s: a fan-out member must be read-only", name)
		}
		// Recovery is per member: each carries its own complete follow-up.
		resume, _ := meta["resumeCommand"].(string)
		mustContain(t, name+" resumeCommand", resume, "envoy run <new-job-name>", "--with @'"+memberDir+"'")
		if got := readFile(t, filepath.Join(memberDir, "result.md")); got != "fake provider result" {
			t.Fatalf("%s result.md = %q", name, got)
		}
		if got := readFile(t, filepath.Join(memberDir, "prompt.md")); got != "fake prompt body\n" {
			t.Fatalf("%s prompt.md = %q", name, got)
		}
	}
	// The shared prompt is recorded once at the fan-out level too, so the
	// group is self-describing even if a member dir is lost.
	if got := readFile(t, filepath.Join(outDir, "prompt.md")); got != "fake prompt body\n" {
		t.Fatalf("group prompt.md = %q", got)
	}

	var group map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(outDir, "group.json"))), &group); err != nil {
		t.Fatalf("group.json: %v", err)
	}
	if group["endedAt"] == nil {
		t.Fatal("group.json must record when the fan-out finished")
	}
	members := group["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("group.json members = %v", members)
	}
	first := members[0].(map[string]any)
	if first["name"] != "codex" || first["outDir"] != filepath.Join(outDir, "codex") {
		t.Fatalf("member roster = %v", first)
	}
	// The manifest is coordinates only. A member status here would be a second
	// copy of what meta.json owns, free to drift from the turn it describes.
	for _, forbidden := range []string{"status", "resultKind", "sessionId"} {
		if _, ok := first[forbidden]; ok {
			t.Fatalf("group.json must not mirror member state, found %q in %v", forbidden, first)
		}
	}

	locks, _ := os.ReadDir(filepath.Join(e.home, ".local", "state", "envoy", "locks"))
	if len(locks) != 0 {
		t.Fatalf("every member's session lock must be released, found %d", len(locks))
	}
}

// The case the whole feature exists for: one member answers, the other does
// not. Exit 6 says both things are true at once, and collection prescribes per
// member — never a group-wide retry that would re-send an accepted prompt.
func TestFanPartialOutcomeCollectsPerMember(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO_CODEX", "success").
		set("ENVOY_FAKE_SCENARIO_CLAUDE", "init-only-hang")
	base := t.TempDir()
	outDir := filepath.Join(base, "20260726-120000-consult")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--with", "claude",
		"--baseline", "deadbeef", "--timeout-min", "0.02")...)
	if res.code != 6 {
		t.Fatalf("exit = %d, want 6 (partial)\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"status: partial — 1 of 2 turns returned a result",
		"member codex: ok · result ",
		"member claude: timeout — the wall-clock cap elapsed, which is not evidence the provider hung",
		"licenses nothing about another",
		"never the whole fan-out",
	)

	// Discovery finds the fan-out as one entry to act on, not two loose jobs.
	pending := runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending stdout", pending.stdout,
		"pending jobs: 1",
		"[group] "+outDir,
		"2 of 2 members still need attention",
		"codex: terminal status ok has not been collected",
		"claude: terminal status timeout has not been collected",
		"next: envoy collect")

	collected := runEnvoy(t, e, "collect", outDir)
	if collected.code != 0 {
		t.Fatalf("collect = %d\n%s", collected.code, collected.stderr)
	}
	mustContain(t, "collect stdout", collected.stdout,
		"fan-out: "+outDir,
		"status: partial — 1 of 2 turns returned a result",
		"members: codex ok · claude timeout",
		"prompt: "+filepath.Join(outDir, "prompt.md"),
		"=== member codex ===",
		"job: "+filepath.Join(outDir, "codex"),
		"fake provider result",
		"=== member claude ===",
		"status: timeout — the wall-clock cap elapsed",
		// the timed-out member's own recovery, not the group's
		"silence is not proof that nothing ran",
		"envoy run <new-job-name> --with @'"+filepath.Join(outDir, "claude")+"'",
		"next: the member results above are usable as they are",
	)
	// The ok member keeps its own single-turn closing line inside its section.
	mustContain(t, "collect stdout", collected.stdout, "result.md above is this turn's return value")
	// Every member reviewed the same range, so the diff is reported once for the
	// fan-out — repeating it per member would bury the findings in duplicates.
	if n := strings.Count(collected.stdout, "git since baseline deadbeef"); n != 1 {
		t.Fatalf("the reviewed range must print once per fan-out, got %d", n)
	}

	for _, name := range []string{"codex", "claude"} {
		if readMeta(t, filepath.Join(outDir, name))["collectedAt"] == nil {
			t.Fatalf("collecting the fan-out must stamp member %s", name)
		}
	}
	pending = runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending after collect", pending.stdout, "pending jobs: 0")
}

// One signal stops every member: the fan-out is one job to interrupt, and no
// member is left running behind a process the caller thinks it stopped.
func TestFanInterruptStopsEveryMember(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "group")
	prompt := writePrompt(t, t.TempDir())

	cmd := exec.Command(binPath, runArgs(prompt, outDir, "--with", "codex", "--with", "claude", "--timeout-min", "5")...)
	cmd.Env = e.build()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Interrupt once both members have provably accepted their prompt.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("members never accepted; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
		accepted := 0
		for _, name := range []string{"codex", "claude"} {
			if data, err := os.ReadFile(filepath.Join(outDir, name, "meta.json")); err == nil &&
				strings.Contains(string(data), `"promptState": "accepted"`) {
				accepted++
			}
		}
		if accepted == 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGINT)
	code := 0
	if ee, ok := cmd.Wait().(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if code != 5 {
		t.Fatalf("exit = %d, want 5\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	mustContain(t, "stdout", stdout.String(),
		"received SIGINT: stopping all 2 turns",
		"status: no-result — none of the 2 turns returned a result")
	for _, name := range []string{"codex", "claude"} {
		meta := readMeta(t, filepath.Join(outDir, name))
		if meta["status"] != "interrupted" || meta["interruptionSignal"] != "SIGINT" {
			t.Fatalf("%s meta = status %v signal %v", name, meta["status"], meta["interruptionSignal"])
		}
	}
}

// The same model dispatched twice is a legitimate fan-out, so members are named
// for what distinguishes them and a repeat is numbered rather than merged.
func TestFanDuplicateMembersGetDistinctJobDirs(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "group")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude:opus", "--with", "claude:opus", "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "member claude-opus: ok", "member claude-opus-2: ok")
	for _, name := range []string{"claude-opus", "claude-opus-2"} {
		if readMeta(t, filepath.Join(outDir, name))["status"] != "ok" {
			t.Fatalf("%s did not run as its own turn", name)
		}
	}
}

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
		"envoy run <job> --prompt-file <F> --with <voice>", // the dispatch form
		"THE LOOP",                // name → dispatch → collect
		"envoy collect review-r1", // the read path, by name
		"nothing to read back",    // why the name is chosen up front
		"used once",               // the reservation rule
		"VOICES",
		"--with codex::high", // effort without a model
		"--with @<job>",      // continuation
		"--with @consult-r1/codex --with claude:opus", // warm beside cold
		"stands alone", // a fan-out reference
		"WHAT A JOB LEAVES BEHIND",
		"FACTS THE FLAGS CANNOT TELL YOU",
		"envoy never substitutes", // no model substitution
		"not a sandbox",           // --allow-write is intent
		"One live\n  turn per session",
		"takes a NEW prompt file", // resume discipline
		"counts healthy work",     // cap semantics
		"EXIT CODES, AND WHAT EACH ONE LICENSES",
		"6 partial",
		"claude: low medium high xhigh max", // rendered from the provider map
	)
	mustNotContain(t, "stdout", res.stdout, "takeover", "watch:", "envoy turn", "envoy fan", "--coordinate-file", "--resume-from", "--with-from", "envoy steer", "envoy jobs")
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
		{[]string{"run", job, "--with", "codex"}, "--prompt-file <path> is required"},
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

func readGroup(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "group.json"))
	if err != nil {
		t.Fatalf("group.json: %v", err)
	}
	var g map[string]any
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("group.json parse: %v\n%s", err, data)
	}
	return g
}

// A finished fan-out continues as a set: every member resumed in its own
// session on one NEW prompt, dispatched and supervised as a new fan-out. The
// roster, each member's session, and the settings come from the original
// fan-out's records, so the caller re-decides nothing per member.
func TestFanResumeFromContinuesEveryMember(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "sess-codex-r1").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "sess-claude-r1")
	r1 := filepath.Join(t.TempDir(), "round1")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, r1, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("round 1 exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}

	// Collecting the finished set offers the set-level follow-up, runnable as
	// printed, with the prompt file left as the placeholder a round 2 must fill.
	col := runEnvoy(t, e, "collect", r1)
	mustContain(t, "collect stdout", col.stdout,
		"resume: envoy run <new-job-name> --with @'"+r1+"' --timeout-min 5 --prompt-file <your-follow-up.md>")

	round2 := filepath.Join(t.TempDir(), "round2.md")
	if err := os.WriteFile(round2, []byte("round-2 prompt body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := filepath.Join(t.TempDir(), "round2-group")
	res = runEnvoy(t, e, "run", r2, "--with", "@"+r1, "--prompt-file", round2, "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("round 2 exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "round 2 stdout", res.stdout,
		"resumed-from: "+r1,
		"member codex: ",
		"member claude-opus: ",
		"status: ok — all 2 turns returned a result",
	)

	// The manifest records the lineage, and the members keep their identities.
	if g := readGroup(t, r2); g["resumedFrom"] != r1 {
		t.Fatalf("resumedFrom = %v, want %s", g["resumedFrom"], r1)
	}
	// Each member's turn is a real resume — the provider argv carries the
	// original session — and the roster's settings carry over.
	codexMeta := readMeta(t, filepath.Join(r2, "codex"))
	mustContain(t, "codex argv", fmt.Sprintf("%v", codexMeta["providerArgv"]),
		"resume", "sess-codex-r1")
	claudeMeta := readMeta(t, filepath.Join(r2, "claude-opus"))
	mustContain(t, "claude argv", fmt.Sprintf("%v", claudeMeta["providerArgv"]),
		"--resume", "sess-claude-r1")
	if claudeMeta["model"] != "opus" {
		t.Fatalf("claude-opus model = %v, want opus carried from the roster", claudeMeta["model"])
	}
	// Each resumed member also records which job its conversation continues.
	if codexMeta["resumedFrom"] != filepath.Join(r1, "codex") ||
		claudeMeta["resumedFrom"] != filepath.Join(r1, "claude-opus") {
		t.Fatalf("member lineage = %v / %v, want the original member dirs under %s",
			codexMeta["resumedFrom"], claudeMeta["resumedFrom"], r1)
	}
	// And every member was sent the NEW prompt, never the original again.
	for _, name := range []string{"codex", "claude-opus"} {
		if got := readFile(t, filepath.Join(r2, name, "prompt.md")); got != "round-2 prompt body\n" {
			t.Fatalf("%s prompt.md = %q", name, got)
		}
	}
}

// The shapes a fan-out reference cannot take are refused with the voices
// that would work, before anything spawns.
func TestFanReferenceRefusals(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())
	fanDir := filepath.Join(t.TempDir(), "fan")
	if r := runEnvoy(t, e, runArgs(prompt, fanDir, "--with", "codex", "--with", "claude", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("fan exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	// A fan-out reference continues every member, so beside another voice
	// it is refused — with each eligible member offered as the voice to name.
	next := filepath.Join(t.TempDir(), "next")
	res := runEnvoy(t, e, runArgs(prompt, next, "--with", "@"+fanDir, "--with", "codex")...)
	if res.code != 3 {
		t.Fatalf("fan ref beside a cold voice: exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "stands alone",
		"--with @'"+filepath.Join(fanDir, "codex")+"'  (codex)",
		"--with @'"+filepath.Join(fanDir, "claude")+"'  (claude)")
	if _, err := os.Stat(next); !os.IsNotExist(err) {
		t.Fatal("a refused run must leave no job dir")
	}

	// Nothing at the path at all.
	res = runEnvoy(t, e, runArgs(prompt, next, "--with", "@"+filepath.Join(t.TempDir(), "nowhere"))...)
	if res.code != 3 {
		t.Fatalf("missing job: exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "no job found there")
}

// The resume set is whole or refused: a member that never published a session
// has no conversation to continue, and quietly dropping that voice would turn
// a two-voice round into a one-voice round without a record of why.
func TestFanResumeFromRefusesIncompleteSet(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO_CODEX", "exit-before-stdin").
		set("ENVOY_FAKE_SCENARIO_CLAUDE", "success")
	r1 := filepath.Join(t.TempDir(), "round1")
	prompt := writePrompt(t, t.TempDir())
	res := runEnvoy(t, e, runArgs(prompt, r1, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5")...)
	if res.code != 6 {
		t.Fatalf("mixed fan exit = %d, want 6\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}

	res = runEnvoy(t, e, runArgs(prompt, filepath.Join(t.TempDir(), "round2"), "--with", "@"+r1)...)
	if res.code != 3 {
		t.Fatalf("incomplete-set continuation exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr,
		"cannot be continued as a set",
		"member codex never published a session id")
}

// --status-only reads coordinates without paying for the result body, and
// deliberately stamps nothing: the result has not been delivered, so pending
// discovery must keep listing the job.
func TestCollectStatusOnlyLeavesTheResultOwed(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	if r := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("turn exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	res := runEnvoy(t, e, "collect", "--status-only", outDir)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"status: ok",
		"resume: envoy run <new-job-name> --with @'"+outDir+"'",
		"next: this was a status check only",
	)
	for _, banned := range []string{"--- result.md ---", "fake provider result"} {
		if strings.Contains(res.stdout, banned) {
			t.Fatalf("status-only must not print %q:\n%s", banned, res.stdout)
		}
	}
	if readMeta(t, outDir)["collectedAt"] != nil {
		t.Fatal("status-only must not stamp collection")
	}
	// The full collect afterwards is still the first collection.
	if r := runEnvoy(t, e, "collect", outDir); r.code != 0 {
		t.Fatalf("full collect exit = %d", r.code)
	}
	if readMeta(t, outDir)["collectedAt"] == nil {
		t.Fatal("full collect must stamp collection")
	}
}

// --result-only hands back an ok turn's payload alone and stamps collection.
// A turn that is not ok prints its full block instead: its status and next
// action are its result, and suppressing them would hand back a payload that
// does not exist.
func TestCollectResultOnly(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	if r := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("turn exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	res := runEnvoy(t, e, "collect", "--result-only", outDir)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "fake provider result")
	for _, banned := range []string{"job:", "status:", "next:", "--- result.md ---"} {
		if strings.Contains(res.stdout, banned) {
			t.Fatalf("result-only must not print %q:\n%s", banned, res.stdout)
		}
	}
	if readMeta(t, outDir)["collectedAt"] == nil {
		t.Fatal("result-only delivered the result, so it must stamp collection")
	}

	// A failed turn has no payload to hand back alone.
	failDir := filepath.Join(t.TempDir(), "failed")
	e2 := newEnv(t).set("ENVOY_FAKE_SCENARIO", "nonzero-final")
	if r := runEnvoy(t, e2, runArgs(prompt, failDir, "--with", "codex", "--timeout-min", "5")...); r.code != 1 {
		t.Fatalf("failed turn exit = %d, want 1", r.code)
	}
	res = runEnvoy(t, e2, "collect", "--result-only", failDir)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "job: "+failDir, "status: failed", "next: ")

	// Both selectors at once select nothing coherent.
	res = runEnvoy(t, e, "collect", "--result-only", "--status-only", outDir)
	if res.code != 3 {
		t.Fatalf("exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "mutually exclusive")
}

// A fan-out's result-only keeps the aggregate line and the member split —
// attribution is the point of a fan-out — and drops the coordinate preamble.
func TestFanCollectResultOnly(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "group")
	prompt := writePrompt(t, t.TempDir())
	if r := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("fan exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	res := runEnvoy(t, e, "collect", "--result-only", outDir)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"status: ok — all 2 turns returned a result",
		"=== member codex ===",
		"=== member claude-opus ===",
		"fake provider result",
	)
	for _, banned := range []string{"fan-out:", "members:", "prompt:", "provider:", "next:"} {
		if strings.Contains(res.stdout, banned) {
			t.Fatalf("group result-only must not print %q:\n%s", banned, res.stdout)
		}
	}
}

// A resumed fan-out starts every member or none — including at dispatch time.
// A member's session held by another live turn (a caller resumed it
// individually, say) must refuse the round before any sibling spawns, not
// strand a partial round after validation passed on historical metadata.
func TestFanResumeFromRefusesWhenASessionIsHeld(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "sess-codex-held").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "sess-claude-free")
	r1 := filepath.Join(t.TempDir(), "round1")
	prompt := writePrompt(t, t.TempDir())
	if res := runEnvoy(t, e, runArgs(prompt, r1, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5")...); res.code != 0 {
		t.Fatalf("round 1 exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	// Another live turn holds the codex member's session, exactly as when a
	// caller resumed that member individually and it is still running.
	lockDir := filepath.Join(e.home, ".local", "state", "envoy", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"pid":%d,"runnerInstanceId":"another-turn","outDir":"%s","startedAt":"2026-07-28T00:00:00.000Z"}`,
		os.Getpid(), t.TempDir())
	if err := os.WriteFile(filepath.Join(lockDir, "sess-codex-held.lock"), []byte(payload+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r2 := filepath.Join(t.TempDir(), "round2")
	res := runEnvoy(t, e, runArgs(prompt, r2, "--with", "@"+r1, "--timeout-min", "5")...)
	if res.code != 3 {
		t.Fatalf("held-session resume exit = %d, want 3\nstdout:\n%s\nstderr:\n%s",
			res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "sess-codex-held")
	if _, err := os.Stat(filepath.Join(r2, "claude-opus", "meta.json")); err == nil {
		t.Fatal("a sibling turn ran while the set was refused")
	}
	// The refusal happened after the name was reserved and before anything
	// ran under it, so the name is given back rather than left occupied.
	if _, err := os.Stat(r2); !os.IsNotExist(err) {
		t.Fatal("a refused round must leave no job dir behind")
	}
}

// "Resumable" has one definition. A member whose turn ended in a session-lock
// conflict does not advertise a resume command of its own, so the set-level
// resume must not advertise or accept it either.
func TestConflictedMemberIsNotSetResumable(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "group")
	codexDir := filepath.Join(dir, "codex")
	opusDir := filepath.Join(dir, "claude-opus")
	for _, d := range []string{codexDir, opusDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cwd := t.TempDir()
	group := fmt.Sprintf(`{"schemaVersion":1,"startedAt":"2026-07-28T00:00:00.000Z","endedAt":"2026-07-28T00:05:00.000Z","cwd":%q,"promptFile":%q,"label":null,"timeoutMin":5,"gitBaseline":null,"outDir":%q,"watchCommand":"","supervisorPid":1,"members":[{"name":"codex","provider":"codex","model":null,"effort":null,"outDir":%q},{"name":"claude-opus","provider":"claude","model":"opus","effort":null,"outDir":%q}]}`,
		cwd, filepath.Join(dir, "prompt.md"), dir, codexDir, opusDir)
	if err := os.WriteFile(filepath.Join(dir, "group.json"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
	okMeta := `{"schemaVersion":4,"status":"ok","provider":"codex","sessionId":"sess-ok","promptState":"accepted","timeoutMin":5,"collectedAt":null}`
	conflictMeta := `{"schemaVersion":4,"status":"failed","provider":"claude","model":"opus","sessionId":"sess-conflict","sessionLockConflict":"session sess-conflict already has a live turn","promptState":"unknown","timeoutMin":5,"collectedAt":null}`
	os.WriteFile(filepath.Join(codexDir, "meta.json"), []byte(okMeta), 0o644)
	os.WriteFile(filepath.Join(opusDir, "meta.json"), []byte(conflictMeta), 0o644)
	os.WriteFile(filepath.Join(codexDir, "result.md"), []byte("codex answer"), 0o644)
	os.WriteFile(filepath.Join(opusDir, "result.md"), []byte("opus partial"), 0o644)

	col := runEnvoy(t, e, "collect", dir)
	if strings.Contains(col.stdout, "--with @'"+dir+"'") {
		t.Fatalf("a conflicted member must suppress the set-level resume line:\n%s", col.stdout)
	}

	prompt := writePrompt(t, t.TempDir())
	res := runEnvoy(t, e, runArgs(prompt, filepath.Join(t.TempDir(), "next"), "--with", "@"+dir)...)
	if res.code != 3 {
		t.Fatalf("conflicted-member resume exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "claude-opus", "session-lock conflict")
}

// collectedAt means the result body reached a caller. A turn that reports ok
// while its result.md is unreadable delivered nothing — the collect must say
// so and leave the job owed, in both full and result-only modes.
func TestCollectDoesNotStampAnOkTurnWithoutItsResult(t *testing.T) {
	for _, args := range [][]string{{"collect"}, {"collect", "--result-only"}} {
		e := newEnv(t)
		dir := filepath.Join(t.TempDir(), "job")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := `{"schemaVersion":4,"status":"ok","provider":"codex","sessionId":"sess-1","promptState":"accepted","timeoutMin":5,"collectedAt":null}`
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}

		res := runEnvoy(t, e, append(args, dir)...)
		if res.code != 0 {
			t.Fatalf("%v exit = %d\nstderr:\n%s", args, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stdout of %v", args), res.stdout, "result.md could not be read")
		// The next line printed here sends the caller to raw.log to recover the
		// payload, so the block must carry the paths: a prescription may not
		// point at files the block withheld. Status alone cannot decide this —
		// this turn is ok and still needs its logs, because its result never
		// reached anyone.
		mustContain(t, fmt.Sprintf("stdout of %v", args), res.stdout,
			"logs: progress "+filepath.Join(dir, "progress.log"),
			"raw "+filepath.Join(dir, "raw.log"),
			"stderr "+filepath.Join(dir, "stderr.log"))
		if readMeta(t, dir)["collectedAt"] != nil {
			t.Fatalf("%v stamped a result it never delivered", args)
		}
	}
}

// A fan-out's closing line may not claim every result is usable while an ok
// member's payload never reached the caller: delivery, not status, is what
// the group's own next action must aggregate.
func TestFanCollectFlagsUndeliveredOkResult(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "group")
	codexDir := filepath.Join(dir, "codex")
	opusDir := filepath.Join(dir, "claude-opus")
	for _, d := range []string{codexDir, opusDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cwd := t.TempDir()
	group := fmt.Sprintf(`{"schemaVersion":1,"startedAt":"2026-07-28T00:00:00.000Z","endedAt":"2026-07-28T00:05:00.000Z","cwd":%q,"promptFile":%q,"label":null,"timeoutMin":5,"gitBaseline":null,"outDir":%q,"watchCommand":"","supervisorPid":1,"members":[{"name":"codex","provider":"codex","model":null,"effort":null,"outDir":%q},{"name":"claude-opus","provider":"claude","model":"opus","effort":null,"outDir":%q}]}`,
		cwd, filepath.Join(dir, "prompt.md"), dir, codexDir, opusDir)
	if err := os.WriteFile(filepath.Join(dir, "group.json"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
	okMeta := `{"schemaVersion":4,"status":"ok","provider":"%s","sessionId":"sess-%s","promptState":"accepted","timeoutMin":5,"collectedAt":null}`
	os.WriteFile(filepath.Join(codexDir, "meta.json"), []byte(fmt.Sprintf(okMeta, "codex", "a")), 0o644)
	os.WriteFile(filepath.Join(opusDir, "meta.json"), []byte(fmt.Sprintf(okMeta, "claude", "b")), 0o644)
	// codex delivered; claude-opus reports ok but its payload is gone.
	os.WriteFile(filepath.Join(codexDir, "result.md"), []byte("codex answer"), 0o644)

	res := runEnvoy(t, e, "collect", dir)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if strings.Contains(res.stdout, "the member results above are this fan-out") {
		t.Fatalf("group closing claims usable results over an undelivered payload:\n%s", res.stdout)
	}
	mustContain(t, "stdout", res.stdout, "could not be read")
	if readMeta(t, opusDir)["collectedAt"] != nil {
		t.Fatal("the undelivered member must stay uncollected")
	}
	if readMeta(t, codexDir)["collectedAt"] == nil {
		t.Fatal("the delivered member must stamp as usual")
	}
}

// A turn whose link dropped and came back is a success: codex's bare `error`
// events are observations, and only turn.failed is a verdict. The tally is
// recorded and printed as a diagnostic line, worded as what the provider
// said — never as "offline", which would be the engine's inference.
func TestCodexReconnectEventsAreObservedNotJudged(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "reconnect-then-success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "status: ok")
	meta := readMeta(t, outDir)
	ce, _ := meta["connectionErrors"].(map[string]any)
	if ce == nil || ce["count"] != float64(2) || ce["firstAt"] == nil || ce["lastAt"] == nil {
		t.Fatalf("connectionErrors must tally the two recognized events with stamps, got %v", meta["connectionErrors"])
	}
	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	mustContain(t, "status block", status.stdout, "provider stream: 2 recognized connection-error events; first observed ")
	mustNotContain(t, "status block", status.stdout, "offline", "network timeout")

	// A clean turn records zero — an observation, distinct from an older
	// engine's meta that never counted.
	clean := filepath.Join(t.TempDir(), "clean")
	runEnvoy(t, newEnv(t).set("ENVOY_FAKE_SCENARIO", "success"), runArgs(prompt, clean, "--with", "codex", "--timeout-min", "5")...)
	cleanStatus := runEnvoy(t, e, "collect", "--status-only", clean)
	mustContain(t, "clean status block", cleanStatus.stdout, "provider stream: no connection-error events recognized by this engine version")
}

// A driver that does not observe connection errors gets no tally: a claude
// block prints no provider-stream line, so a zero can never claim a link held
// that nothing watched.
func TestClaudeBlockCarriesNoConnectionTally(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	if res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude:opus", "--timeout-min", "5")...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if v, has := readMeta(t, outDir)["connectionErrors"]; !has || v != nil {
		t.Fatalf("claude meta must record connectionErrors: null, got %v (present %v)", v, has)
	}
	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	mustNotContain(t, "claude status block", status.stdout, "provider stream:")
}

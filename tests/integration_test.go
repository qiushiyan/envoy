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

func turnArgs(prompt, outDir string, extra ...string) []string {
	args := []string{"turn", "--prompt-file", prompt, "--out-dir", outDir}
	return append(args, extra...)
}

func TestCodexSuccess(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "5", "--label", "consult")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"out-dir: "+outDir,
		"provider: codex · model (provider default) · effort (provider default) · hard cap 5m",
		"watch: tail -f",
		"session: fake-session-id",
		"takeover-after-terminal: codex resume fake-session-id",
		"next: return now; wait for the native background-task notification",
		"status: ok",
		"takeover: codex resume fake-session-id",
		"next: Collect and verify this job: envoy collect",
	)
	if got := readFile(t, filepath.Join(outDir, "result.md")); got != "fake provider result" {
		t.Fatalf("result.md = %q", got)
	}
	if got := readFile(t, filepath.Join(outDir, "prompt.md")); got != "fake prompt body\n" {
		t.Fatalf("prompt.md = %q", got)
	}

	meta := readMeta(t, outDir)
	if meta["status"] != "ok" || meta["promptState"] != "accepted" {
		t.Fatalf("meta = status %v prompt %v", meta["status"], meta["promptState"])
	}
	if meta["promptStateEvidence"] != "codex thread.started" {
		t.Fatalf("evidence = %v", meta["promptStateEvidence"])
	}
	if meta["resumeArgs"] != "--resume fake-session-id --timeout-min 5" {
		t.Fatalf("resumeArgs = %v", meta["resumeArgs"])
	}
	tokens := meta["tokens"].(map[string]any)
	if tokens["input"] != float64(13) || tokens["cachedInput"] != float64(5) || tokens["reasoningOutput"] != float64(3) {
		t.Fatalf("tokens = %v", tokens)
	}
	if v, ok := meta["collectedAt"]; !ok || v != nil {
		t.Fatalf("collectedAt must be explicit null before collection, got %v (present=%v)", v, ok)
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

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "claude", "--timeout-min", "5")...)
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
	if meta["takeoverCommand"] != "claude --resume fake-session-id" {
		t.Fatalf("takeover = %v", meta["takeoverCommand"])
	}
	if meta["costUsd"] != float64(0.01) {
		t.Fatalf("cost = %v", meta["costUsd"])
	}
	tokens := meta["tokens"].(map[string]any)
	if tokens["cacheRead"] != float64(4) || tokens["cacheCreation"] != float64(2) {
		t.Fatalf("tokens = %v", tokens)
	}
	mustContain(t, "terminal stdout", res.stdout, "session: fake-session-id", "takeover: claude --resume fake-session-id")
}

func TestClaudePartialFailure(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "partial-failure")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "claude", "--timeout-min", "5")...)
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

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "0.02")...)
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "timeout" || meta["promptState"] != "accepted" {
		t.Fatalf("meta = status %v prompt %v", meta["status"], meta["promptState"])
	}
	mustContain(t, "error", meta["error"].(string),
		"hard wall-clock cap ended this codex turn",
		"not evidence that the provider hung")
	mustContain(t, "recoveryAction", meta["recoveryAction"].(string),
		"--resume fake-session-id --timeout-min 0.02",
		"Do not redispatch the original prompt")
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")), "# Turn timeout")
}

func TestCodexTerminalEnvelopeWinsDuringCleanup(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "terminal-success-then-hang")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "0.02")...)
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
	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "claude", "--timeout-min", "0.02")...)
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

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "claude", "--timeout-min", "0.02")...)
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

func TestResumeLockConflictFailsFast(t *testing.T) {
	e := newEnv(t)
	lockDir := filepath.Join(e.home, ".local", "state", "envoy", "locks")
	os.MkdirAll(lockDir, 0o755)
	payload := fmt.Sprintf(`{"pid":%d,"runnerInstanceId":"other","outDir":"/tmp/live-job","startedAt":"2026-07-25T00:00:00.000Z"}`, os.Getpid())
	os.WriteFile(filepath.Join(lockDir, "locked-session.lock"), []byte(payload), 0o644)

	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "codex", "--resume", "locked-session")...)
	if res.code != 3 {
		t.Fatalf("exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "lock error:", "already has a live turn", "/tmp/live-job")
	if _, err := os.Stat(filepath.Join(outDir, "prompt.md")); !os.IsNotExist(err) {
		t.Fatal("a refused resume must not touch job artifacts")
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
	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "5")...)
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "lock error:")
	meta := readMeta(t, outDir)
	if meta["sessionLockConflict"] == nil {
		t.Fatal("meta must record the lock conflict")
	}
	if meta["resumeArgs"] != nil || meta["takeoverCommand"] != nil {
		t.Fatalf("a conflicted session must suppress resume/takeover coordinates: %v %v",
			meta["resumeArgs"], meta["takeoverCommand"])
	}
	if meta["promptStateEvidence"] != "codex thread.started with conflicting session lock" {
		t.Fatalf("evidence = %v", meta["promptStateEvidence"])
	}
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")),
		"# Turn infra", "already locked")
	if strings.Contains(res.stdout, "takeover-after-terminal:") {
		t.Fatal("a conflicted fresh session must not advertise takeover")
	}
}

func TestInterruptRecordsPartialAndResume(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	cmd := exec.Command(binPath, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "5")...)
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
	mustContain(t, "recoveryAction", meta["recoveryAction"].(string), "--resume fake-session-id --timeout-min 5")
}

func TestCollectStampsAndPendingDiscovers(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	base := t.TempDir()
	outDir := filepath.Join(base, "20260725-120000-consult")
	prompt := writePrompt(t, t.TempDir())

	if res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "5")...); res.code != 0 {
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
		"provider: codex",
		"tokens: input 13 · cachedInput 5 · output 8 · reasoningOutput 3",
		"session: fake-session-id",
		"resume: --resume fake-session-id --timeout-min 5",
		"--- result.md ---",
		"fake provider result",
		"next: Use this result in the invoking skill's verification, judgment, or synthesis step.")

	meta := readMeta(t, outDir)
	if meta["collectedAt"] == nil {
		t.Fatal("collection must stamp collectedAt")
	}

	pending = runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending after collect", pending.stdout, "pending jobs: 0", "no recovery action is needed")
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
		"resumeArgs": "--resume dead-session --timeout-min 180",
		"runnerPid":  4194304, "providerPid": 4194304, "providerPgid": 4194304,
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
		"status: abandoned",
		"The envoy runner ended without publishing a terminal result",
		"continue the same session with --resume dead-session --timeout-min 180")
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

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "5")...)
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "infra" || meta["childExitCode"] != float64(23) {
		t.Fatalf("meta = status %v exit %v", meta["status"], meta["childExitCode"])
	}
	mustContain(t, "error", meta["error"].(string), "fake provider exited before reading its prompt")
}

func TestDefaultStorageIsCentralAndHidden(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())

	// Dispatch without --out-dir: the job must land in the central store,
	// never inside the project tree.
	res := runEnvoyIn(t, e, project, "turn", "--prompt-file", prompt,
		"--provider", "codex", "--timeout-min", "5", "--label", "consult")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	var outDir string
	for _, line := range strings.Split(res.stdout, "\n") {
		if after, ok := strings.CutPrefix(line, "out-dir: "); ok {
			outDir = after
			break
		}
	}
	jobsRoot := filepath.Join(e.home, ".local", "state", "envoy", "jobs")
	if !strings.HasPrefix(outDir, jobsRoot+string(filepath.Separator)) {
		t.Fatalf("out-dir %q must live under the central store %q", outDir, jobsRoot)
	}
	entries, _ := os.ReadDir(project)
	if len(entries) != 0 {
		t.Fatalf("the project tree must stay untouched, found %v", entries)
	}

	// A caller collecting from the project needs no path at all.
	collected := runEnvoyIn(t, e, project, "collect")
	if collected.code != 0 {
		t.Fatalf("collect = %d\n%s", collected.code, collected.stderr)
	}
	mustContain(t, "collect stdout", collected.stdout,
		"job: "+outDir, "status: ok", "fake provider result")

	// And pending resolves the same project store from cwd alone.
	pending := runEnvoyIn(t, e, project, "pending")
	mustContain(t, "pending stdout", pending.stdout, "pending jobs: 0")
}

func TestUsageErrors(t *testing.T) {
	e := newEnv(t)
	prompt := writePrompt(t, t.TempDir())
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"turn", "--prompt-file", prompt}, "--provider <claude|codex> is required"},
		{[]string{"turn", "--provider", "gemini", "--prompt-file", prompt}, "must be claude or codex"},
		{[]string{"turn", "--provider", "codex"}, "--prompt-file <path> is required"},
		{[]string{"turn", "--provider", "claude", "--prompt-file", prompt, "--effort", "minimal"}, "claude has no 'minimal'"},
		{[]string{"turn", "--provider", "codex", "--prompt-file", prompt, "--max-budget-usd", "1"}, "exists only on claude"},
		{[]string{"turn", "--provider", "codex", "--prompt-file", prompt, "--timeout-min", "-1"}, "--timeout-min must be a number >= 0"},
		{[]string{"nonsense"}, "unknown command"},
	}
	for _, c := range cases {
		res := runEnvoy(t, e, c.args...)
		if res.code != 3 {
			t.Fatalf("%v: exit = %d, want 3", c.args, res.code)
		}
		mustContain(t, fmt.Sprintf("stderr of %v", c.args), res.stderr, c.want)
	}
}

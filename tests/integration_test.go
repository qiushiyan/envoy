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
	receivedPrompt := filepath.Join(t.TempDir(), "received-prompt.txt")
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_PROMPT_FILE", receivedPrompt)
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
		"next: let this command run to completion, then collect the job: envoy collect",
		"tailing the logs is observation only",
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
	// has to assemble — and it carries the settings this turn was dispatched with.
	resume, _ := meta["resumeCommand"].(string)
	for _, want := range []string{"envoy turn", "--provider codex", "--resume fake-session-id", "--timeout-min 5", "--prompt-file <your-follow-up.md>"} {
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

	// The provider announced its resolved model; the engine records the
	// observation without inferring anything from the (absent) request.
	if meta["providerReportedModel"] != "fake-claude-model" {
		t.Fatalf("providerReportedModel = %v", meta["providerReportedModel"])
	}
	mustContain(t, "progress.log", readFile(t, filepath.Join(outDir, "progress.log")),
		"state=provider-initialized session=fake-session-id model=fake-claude-model")
	collected := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "collect stdout", collected.stdout,
		"provider: claude · model (provider default, ran fake-claude-model) · effort (provider default)")
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
		"wall-clock cap ended this codex turn",
		"not evidence the provider hung")
	mustContain(t, "recoveryAction", meta["recoveryAction"].(string),
		"--resume fake-session-id", "--timeout-min 0.02",
		"would repeat work that already happened")
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
	if meta["resumeCommand"] != nil || meta["takeoverCommand"] != nil {
		t.Fatalf("a conflicted session must suppress resume/takeover coordinates: %v %v",
			meta["resumeCommand"], meta["takeoverCommand"])
	}
	if meta["promptStateEvidence"] != "codex thread.started with conflicting session lock" {
		t.Fatalf("evidence = %v", meta["promptStateEvidence"])
	}
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")),
		"# Turn infra", "another turn already holds")
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
	mustContain(t, "recoveryAction", meta["recoveryAction"].(string),
		"envoy turn", "--resume fake-session-id", "--timeout-min 5")
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
		"resume: envoy turn --provider codex --resume fake-session-id",
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
		"--resume dead-session")
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

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "5")...)
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

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "claude", "--timeout-min", "5")...)
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

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "5")...)
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

	res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "claude", "--timeout-min", "0.02")...)
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

func fanArgs(prompt, outDir string, extra ...string) []string {
	args := []string{"fan", "--prompt-file", prompt, "--out-dir", outDir}
	return append(args, extra...)
}

// The fan-out contract: one command, one completion, one directory — and
// underneath it, members that are ordinary turns in every respect.
func TestFanDispatchesEveryMemberAsAnOrdinaryTurn(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "group")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, fanArgs(prompt, outDir, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5", "--label", "consult")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"out-dir: "+outDir,
		"fan-out: 2 turns · one prompt · hard cap 5m each",
		"member codex: model (provider default) · effort (provider default) · out-dir "+filepath.Join(outDir, "codex"),
		"member claude-opus: model opus · effort (provider default) · out-dir "+filepath.Join(outDir, "claude-opus"),
		"watch: tail -f",
		"next: let this command run to completion — it exits once every member is done",
		"nothing to track per member",
		"status: ok — all 2 turns returned a result",
		"member codex: ok · result ",
		"member claude-opus: ok · result ",
		"next: Collect the fan-out: envoy collect ",
	)
	// A member's stdout block would interleave with its siblings', so the
	// single-turn coordinates stay in each member's own meta.json.
	if strings.Contains(res.stdout, "takeover-after-terminal:") {
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
		mustContain(t, name+" resumeCommand", resume, "envoy turn", "--provider "+wantProvider, "--resume ")
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

	res := runEnvoy(t, e, fanArgs(prompt, outDir, "--with", "codex", "--with", "claude",
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
		"envoy turn --provider claude --resume ",
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

	cmd := exec.Command(binPath, fanArgs(prompt, outDir, "--with", "codex", "--with", "claude", "--timeout-min", "5")...)
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

	res := runEnvoy(t, e, fanArgs(prompt, outDir, "--with", "claude:opus", "--with", "claude:opus", "--timeout-min", "5")...)
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

// A turn flag aimed at a fan-out is refused in the fan-out's own terms: each
// refusal names the alternative, because the caller's next move is a different
// command, not a different prompt.
func TestFanRefusesTurnOnlyFlags(t *testing.T) {
	e := newEnv(t)
	prompt := writePrompt(t, t.TempDir())
	both := []string{"--with", "codex", "--with", "claude"}
	cases := []struct {
		extra []string
		want  string
	}{
		{[]string{"--allow-write"}, "its members share one working tree"},
		{[]string{"--resume", "sess-1"}, "a session id names one conversation"},
		{[]string{"--model", "opus"}, "--model is not available on a fan-out"},
		{[]string{"--effort", "high"}, "Put it in the member spec instead"},
		{[]string{"--provider", "codex"}, "--provider is not available on a fan-out"},
	}
	for _, c := range cases {
		args := append([]string{"fan", "--prompt-file", prompt}, both...)
		res := runEnvoy(t, e, append(args, c.extra...)...)
		if res.code != 3 {
			t.Fatalf("%v: exit = %d, want 3\nstderr:\n%s", c.extra, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stderr of %v", c.extra), res.stderr, c.want)
	}

	specCases := []struct {
		args []string
		want string
	}{
		{[]string{"fan", "--prompt-file", prompt, "--with", "codex"}, "a fan-out needs at least two members"},
		{[]string{"fan", "--prompt-file", prompt, "--with", "codex", "--with", "gemini"}, "must name provider claude or codex"},
		{[]string{"fan", "--prompt-file", prompt, "--with", "codex", "--with", "claude:opus:minimal"}, "claude has no 'minimal'"},
		{[]string{"fan", "--prompt-file", prompt, "--with", "codex", "--with", "claude:opus:high:extra"}, "has too many fields"},
		{[]string{"fan", "--prompt-file", prompt, "--with", "codex", "--with", "claude", "codex"}, "each member is passed as --with"},
		{[]string{"fan", "--with", "codex", "--with", "claude"}, "--prompt-file <path> is required"},
	}
	for _, c := range specCases {
		res := runEnvoy(t, e, c.args...)
		if res.code != 3 {
			t.Fatalf("%v: exit = %d, want 3\nstderr:\n%s", c.args, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stderr of %v", c.args), res.stderr, c.want)
	}
}

// An explicit help request is not a usage error: agents read exit codes.
//
// The page is also the tool description a caller reads before driving envoy,
// so it has to stand alone: the loop, what a turn leaves behind, the facts
// that decide whether a retry is safe, and what each exit code licenses. A
// caller that has read this should need no other instructions.
func TestHelpIsSelfSufficient(t *testing.T) {
	res := runEnvoy(t, newEnv(t), "collect", "--help")
	if res.code != 0 {
		t.Fatalf("collect --help exit = %d, want 0\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"envoy turn --provider",            // the dispatch form
		"THE LOOP",                         // dispatch → run to completion → collect
		"envoy collect",                    // the read path
		"shows progress, never completion", // a quiet log is not done
		"WHAT A TURN LEAVES BEHIND",        // the artifacts
		"BEFORE YOU DISPATCH",              // the facts a caller cannot discover
		"envoy never substitutes",          // no model substitution
		"not a sandbox",                    // --allow-write is intent
		"One live turn per session",        // the concurrency rule
		"takes a NEW prompt file",          // resume discipline
		"counts healthy work",              // cap semantics
		"EXIT CODES, AND WHAT EACH ONE LICENSES",
		"claude: low medium high xhigh max", // rendered from the provider map
		// the fan-out: what it is for, how a member is spelled, and the two
		// facts a caller cannot discover from the flags
		"ONE PROMPT, SEVERAL MODELS",
		"envoy fan --prompt-file",
		"--with codex --with claude:opus",
		"recovery stays per member",
		"A fan-out is read-only",
		"6 partial",
	)
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

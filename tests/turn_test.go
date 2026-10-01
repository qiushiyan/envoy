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

// One turn's lifecycle end to end: success, provider failure, the cap, an
// interrupt, stubborn grandchildren, the terminal-envelope race, and the
// stream observations a turn records on the way.

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
	if got := stringsFromMeta(t, meta, "commandPrefix"); fmt.Sprint(got) != "[codex]" {
		t.Fatalf("default command prefix = %q", got)
	}
	mustNotContain(t, "default dispatch", res.stdout, "launcher:")
	if meta["status"] != "ok" || meta["promptState"] != "accepted" {
		t.Fatalf("meta = status %v prompt %v", meta["status"], meta["promptState"])
	}
	if meta["promptStateEvidence"] != "codex thread.started" {
		t.Fatalf("evidence = %v", meta["promptStateEvidence"])
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
	if got := stringsFromMeta(t, meta, "commandPrefix"); fmt.Sprint(got) != "[claude]" {
		t.Fatalf("default command prefix = %q", got)
	}
	mustNotContain(t, "default dispatch", res.stdout, "launcher:")
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
	mustContain(t, "error", failureText(t, outDir),
		"wall-clock cap ended this codex turn",
		"not evidence the provider hung")
	// The record holds the observation the sentence is worded from, never the
	// sentence: a stored job reads in whatever vocabulary collects it.
	failure, _ := meta["failure"].(map[string]any)
	stream, _ := failure["stream"].(map[string]any)
	if failure["cause"] != "timeout" || stream["events"] != float64(1) || stream["lastEvent"] != "thread.started" {
		t.Fatalf("failure record = %v", meta["failure"])
	}
	if _, worded := meta["error"]; worded {
		t.Fatal("meta.json must not carry a worded error")
	}
	// This scenario is the shape two real timeouts took: the provider announced
	// its thread and then streamed nothing until the cap. The envelope must
	// carry that observation, because the caller cannot otherwise tell it from
	// a turn that worked right up to the deadline — and the follow-up each one
	// deserves is different.
	mustContain(t, "error", failureText(t, outDir),
		"the stream had been quiet for", "after 1 event (last: thread.started)")
	// The prescription is rendered from the records at collect time: the
	// prompt was accepted, so the one move offered is to continue the session.
	col := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "collect next", col.stdout,
		"next: The provider accepted this prompt",
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
	mustContain(t, "error", failureText(t, outDir), "after 1 event (last: thread.started)")
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
	mustContain(t, "error", failureText(t, outDir),
		"wall-clock cap ended this codex turn",
		"The provider wrote nothing at all before the cap — no output on either stream, and no events.")
	if strings.Contains(failureText(t, outDir), "quiet for") {
		t.Fatalf("no output means no quiet interval to report: %v", failureText(t, outDir))
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
	errText := failureText(t, outDir)
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

// A second interrupt is the caller saying "stop waiting": the tree is killed
// now rather than after the SIGKILL grace, a grandchild that ignores every
// polite signal included, and the turn keeps the interrupt that started it.
func TestASecondInterruptKillsTheTreeNow(t *testing.T) {
	pidFile, readyFile := filepath.Join(t.TempDir(), "grandchild.pid"), filepath.Join(t.TempDir(), "ready")
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "hang-with-stubborn-grandchild-only").
		set("ENVOY_FAKE_GRANDCHILD_PID_FILE", pidFile).
		set("ENVOY_FAKE_GRANDCHILD_READY_FILE", readyFile).
		// A grace far past the test's own wait: only the second interrupt
		// can end this stop in time.
		set("ENVOY_SIGKILL_AFTER_MS", "120000")
	outDir := filepath.Join(t.TempDir(), "job")
	cmd := exec.Command(binPath, runArgs(writePrompt(t, t.TempDir()), outDir, "--with", "codex", "--timeout-min", "5")...)
	cmd.Env = e.build()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		var pid int
		if data, err := os.ReadFile(pidFile); err == nil {
			fmt.Sscanf(string(data), "%d", &pid)
		}
		if pid > 0 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		cmd.Process.Kill()
	})
	waitUntil := func(what string, ok func() bool) {
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s never happened", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitUntil("grandchild ready", func() bool { _, err := os.Stat(readyFile); return err == nil })
	cmd.Process.Signal(syscall.SIGINT)
	waitUntil("the stop", func() bool {
		data, _ := os.ReadFile(filepath.Join(outDir, "progress.log"))
		return strings.Contains(string(data), "state=stopping")
	})
	cmd.Process.Signal(syscall.SIGINT)

	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 5 {
			t.Fatalf("exit %v, want 5", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a second interrupt must not wait out the SIGKILL grace")
	}
	var grandchild int
	fmt.Sscanf(readFile(t, pidFile), "%d", &grandchild)
	waitUntil(fmt.Sprintf("grandchild %d killed", grandchild), func() bool { return syscall.Kill(grandchild, 0) == syscall.ESRCH })
	meta := readMeta(t, outDir)
	if meta["status"] != "interrupted" || meta["interruptionSignal"] != "SIGINT" {
		t.Fatalf("meta = status %v signal %v", meta["status"], meta["interruptionSignal"])
	}
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
	mustContain(t, "error", failureText(t, outDir), "stopped codex after receiving SIGINT")
	col := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "collect next", col.stdout,
		"envoy run <new-job-name>", "--with @'"+outDir+"'", "--timeout-min 5")
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
	mustContain(t, "error", failureText(t, outDir), "fake provider exited before reading its prompt")
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
	mustContain(t, "error", failureText(t, outDir), "Codex exited with code 7 after producing a response")
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
	col := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "collect next", col.stdout, "silence is not proof", "only once they show it never began")
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

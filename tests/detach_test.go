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

// startLeading starts `envoy run` as the leader of a process group of its
// own, the way a harness starts a background task, so the test can stop it
// the way the harness does: by signalling that group.
func startLeading(t *testing.T, e *env, dir string, args ...string) (*exec.Cmd, *strings.Builder) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env, cmd.Dir = e.build(), dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, &stderr
}

// tearDown stops a dispatch the way Claude Code ends a session or stops a
// background task: SIGTERM to the task's process group, then SIGKILL.
func tearDown(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	cmd.Wait()
}

// The caller's environment going away stops only the waiting. The turn its
// dispatch started runs on to its own end, records what happened, and a wait
// tells the caller when it ended.
func TestATurnOutlivesItsCaller(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "delayed-success").set("ENVOY_FAKE_DELAY_MS", "1500")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	cmd, stderr := startLeading(t, e, "", runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "1")...)
	t.Cleanup(func() { stopTurns(outDir) })
	waitForStatus(t, outDir, "running")

	tearDown(t, cmd)
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() {
		t.Fatalf("the dispatch must end by the signal that stopped it, got %v", cmd.ProcessState)
	}
	mustContain(t, "dispatch stderr", stderr.String(),
		"received SIGTERM, so this command stopped waiting; the turn keeps running",
		"envoy wait '"+outDir+"'",
	)

	res := runEnvoy(t, e, "wait", outDir)
	if res.code != 0 {
		t.Fatalf("wait exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "wait stdout", res.stdout, "status: ok")
	meta := readMeta(t, outDir)
	if meta["status"] != "ok" || meta["interruptionSignal"] != nil {
		t.Fatalf("turn = status %v, interruption %v; want ok and none", meta["status"], meta["interruptionSignal"])
	}
	if meta["collectedAt"] != nil {
		t.Fatalf("nothing collected it, yet collectedAt = %v", meta["collectedAt"])
	}
}

// A fan-out runs its members in one process of its own, so it outlives its
// caller as a whole.
func TestAFanOutOutlivesItsCaller(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_DELAY_MS", "1500").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "codex-session").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "claude-session")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	cmd, _ := startLeading(t, e, "", runArgs(prompt, outDir, "--with", "codex", "--with", "claude:opus", "--timeout-min", "1")...)
	t.Cleanup(func() { stopTurns(outDir) })
	waitForStatus(t, filepath.Join(outDir, "codex"), "running")
	waitForStatus(t, filepath.Join(outDir, "claude-opus"), "running")

	tearDown(t, cmd)
	res := runEnvoy(t, e, "wait", outDir)
	if res.code != 0 {
		t.Fatalf("wait exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "wait stdout", res.stdout, "member codex: ok", "member claude-opus: ok")
}

// With the dispatch's own process gone, collect is where a running turn's
// stop is found, and that command stops it the way Ctrl-C on the dispatch
// would have.
func TestTheStopCommandStopsADetachedTurn(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	cmd, _ := startLeading(t, e, "", runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "1")...)
	t.Cleanup(func() { stopTurns(outDir) })
	waitForStatus(t, outDir, "running")
	tearDown(t, cmd)

	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	runner := int(readMeta(t, outDir)["runnerPid"].(float64))
	stop := fmt.Sprintf("stop: kill -INT %d", runner)
	mustContain(t, "collect stdout", status.stdout, stop, "envoy wait '"+outDir+"'")
	full := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "plain collect", full.stdout, stop)

	if err := exec.Command("sh", "-c", strings.TrimPrefix(stop, "stop: ")).Run(); err != nil {
		t.Fatal(err)
	}
	res := runEnvoy(t, e, "wait", outDir)
	if res.code != 5 {
		t.Fatalf("wait exit = %d, want 5\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	if meta := readMeta(t, outDir); meta["status"] != "interrupted" || meta["interruptionSignal"] != "SIGINT" {
		t.Fatalf("turn = status %v, interruption %v; want interrupted by SIGINT", meta["status"], meta["interruptionSignal"])
	}
}

// A turn still running is listed to the caller that may have lost its
// dispatch, with the wait that tells it when the turn ends, and to no other
// caller: another session's healthy turn is not theirs to wait on.
func TestPendingListsALiveTurnOnlyToItsCaller(t *testing.T) {
	base := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	mine, theirs := base.as("session-a", ""), base.as("session-b", "")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	cmd, _ := startLeading(t, mine, project, "run", "review-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "1")
	var outDir string
	deadline := time.Now().Add(10 * time.Second)
	for outDir == "" && time.Now().Before(deadline) {
		r := runEnvoyIn(t, mine, project, "collect", "--status-only", "review-r1")
		if dir, ok := strings.CutPrefix(strings.SplitN(r.stdout, "\n", 2)[0], "job: "); ok && strings.Contains(r.stdout, "status: running") {
			outDir = dir
		}
		time.Sleep(20 * time.Millisecond)
	}
	if outDir == "" {
		killDispatch(cmd, "")
		t.Fatal("the turn never showed as running")
	}
	t.Cleanup(func() { stopTurns(outDir) })
	tearDown(t, cmd)

	listed := runEnvoyIn(t, mine, project, "pending")
	runner := int(readMeta(t, outDir)["runnerPid"].(float64))
	mustContain(t, "pending (its caller)", listed.stdout, "[running] "+outDir, "envoy wait '"+outDir+"'", fmt.Sprintf("stop: kill -INT %d", runner))
	other := runEnvoyIn(t, theirs, project, "pending")
	mustNotContain(t, "pending (another caller)", other.stdout, outDir)
	mustContain(t, "pending (another caller)", other.stdout, "pending jobs: 0")
}

// Refusals before anything runs reach the caller through the waiter, with
// their exit code.
func TestARefusalReachesTheCallerThroughTheWaiter(t *testing.T) {
	res := runEnvoy(t, newEnv(t), "run", "job", "--with", "codex")
	if res.code != 3 || !strings.Contains(res.stderr, "usage error: --with codex has no prompt") {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
}

// The provider starts with only its own stdin, stdout and stderr. A
// descriptor of the dispatch's — the report it makes to its waiter, its
// claim on the job dir — held by a provider would outlive the dispatch in
// any grandchild the provider leaves behind, and the waiter would wait on
// that grandchild instead of on the turn.
func TestTheProviderInheritsNoDescriptorOfTheDispatch(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "open-fds")
	launcher := filepath.Join(dir, "fd-probe")
	script := "#!/bin/sh\nfor fd in 3 4 5 6 7 8 9; do [ -e /dev/fd/$fd ] && echo $fd >> " + record + "; done\ntouch " + record + "\nexec \"$@\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t).set("ENVOY_CODEX_CMD", launcher+" codex")
	outDir := filepath.Join(t.TempDir(), "job")
	res := runEnvoy(t, e, runArgs(writePrompt(t, t.TempDir()), outDir, "--with", "codex", "--timeout-min", "1")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	if open := readFile(t, record); open != "" {
		t.Fatalf("the provider inherited descriptors beyond stdio: %q", open)
	}
}

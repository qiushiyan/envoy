package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForStatus polls a job's meta.json until it records want.
func waitForStatus(t *testing.T, outDir, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(outDir, "meta.json")); err == nil &&
			strings.Contains(string(data), `"status": "`+want+`"`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never recorded status %q", outDir, want)
}

// A wait started while the turn runs returns once the turn's own process has
// finished it, with the block and exit code its dispatch ends on — and
// delivers nothing, so the job stays owed to a collect.
func TestWaitReturnsWhenTheTurnEnds(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "delayed-success").set("ENVOY_FAKE_DELAY_MS", "1500")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	dispatch := exec.Command(binPath, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "1")...)
	dispatch.Env = e.build()
	if err := dispatch.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dispatch.Wait() })
	waitForStatus(t, outDir, "running")

	res := runEnvoy(t, e, "wait", outDir)
	if res.code != 0 {
		t.Fatalf("wait exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	if meta := readMeta(t, outDir); meta["status"] != "ok" {
		t.Fatalf("wait returned while the turn was %v", meta["status"])
	}
	mustContain(t, "wait stdout", res.stdout,
		"job: "+outDir,
		"status: ok",
		"session: fake-session-id",
		"next: Collect and verify this job: envoy collect",
	)
	mustNotContain(t, "wait stdout", res.stdout, "fake provider result")
	if v := readMeta(t, outDir)["collectedAt"]; v != nil {
		t.Fatalf("a wait must not stamp collection, got collectedAt %v", v)
	}
}

// A finished job is waited on at once, and the code is the one its dispatch
// exited with, so a caller that lost the dispatch's exit reads the same thing.
func TestWaitOnAFinishedJobReturnsItsDispatchCode(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "partial-failure")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	run := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude", "--timeout-min", "1")...)
	if run.code != 1 {
		t.Fatalf("dispatch exit = %d, want 1\n%s", run.code, run.stdout)
	}

	start := time.Now()
	res := runEnvoy(t, e, "wait", outDir)
	if res.code != run.code {
		t.Fatalf("wait exit = %d, want the dispatch's %d\n%s%s", res.code, run.code, res.stdout, res.stderr)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("waiting on a finished job took %s", took)
	}
	mustContain(t, "wait stdout", res.stdout, "status: failed", "next: Collect and verify this job")
	if v := readMeta(t, outDir)["collectedAt"]; v != nil {
		t.Fatalf("a wait must not stamp collection, got collectedAt %v", v)
	}
}

// A fan-out is waited on as a whole and ends on the fan-out's own block.
func TestWaitOnAFanOut(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SESSION_ID_CODEX", "codex-session").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "claude-session")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	run := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--with", "claude:opus", "--timeout-min", "1")...)
	if run.code != 0 {
		t.Fatalf("dispatch exit = %d\n%s%s", run.code, run.stdout, run.stderr)
	}
	res := runEnvoy(t, e, "wait", outDir)
	if res.code != 0 {
		t.Fatalf("wait exit = %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "wait stdout", res.stdout,
		"job: "+outDir,
		"status: ok — all 2 turns returned a result",
		"member codex: ok",
		"member claude-opus: ok",
		"group: "+filepath.Join(outDir, "group.json"),
	)
}

// A fan-out whose members ended differently is waited on to the fan-out's
// own code, and the wait leaves every member owed to a collect.
func TestWaitOnAPartialFanOut(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO_CLAUDE", "partial-failure").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "codex-session").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "claude-session")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	run := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--with", "claude:opus", "--timeout-min", "1")...)
	if run.code != 6 {
		t.Fatalf("dispatch exit = %d, want 6\n%s%s", run.code, run.stdout, run.stderr)
	}
	res := runEnvoy(t, e, "wait", outDir)
	if res.code != run.code {
		t.Fatalf("wait exit = %d, want the dispatch's %d\n%s%s", res.code, run.code, res.stdout, res.stderr)
	}
	mustContain(t, "wait stdout", res.stdout, "member codex: ok", "member claude-opus: failed")
	for _, m := range []string{"codex", "claude-opus"} {
		if v := readMeta(t, filepath.Join(outDir, m))["collectedAt"]; v != nil {
			t.Fatalf("a wait must not stamp member %s collected, got %v", m, v)
		}
	}
}

func TestWaitNeedsAJob(t *testing.T) {
	res := runEnvoy(t, newEnv(t), "wait")
	if res.code != 3 || !strings.Contains(res.stderr, "wait takes the job to wait for") {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
}

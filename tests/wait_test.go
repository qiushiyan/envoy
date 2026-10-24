package integration

import (
	"bufio"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A finished job is waited on at once, and ends on exactly the block and the
// code its dispatch ended on, so a caller that lost the dispatch reads the
// same thing — and delivers nothing, leaving the job owed to a collect.
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
	if ending := strings.TrimPrefix(res.stdout, "job: "+outDir+"\n"); ending == res.stdout || !strings.HasSuffix(run.stdout, ending) {
		t.Fatalf("the wait must name its job, then print the block its dispatch ended on:\n%s\ndispatch:\n%s", res.stdout, run.stdout)
	}
	if v := readMeta(t, outDir)["collectedAt"]; v != nil {
		t.Fatalf("a wait must not stamp collection, got collectedAt %v", v)
	}
}

// A fan-out is waited on as a whole: a wait on one whose members ended
// differently ends on the fan-out's own block and code, as its dispatch did,
// and leaves every member owed to a collect.
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
	if ending := strings.TrimPrefix(res.stdout, "job: "+outDir+"\n"); ending == res.stdout || !strings.HasSuffix(run.stdout, ending) {
		t.Fatalf("the wait must name its job, then print the block its dispatch ended on:\n%s\ndispatch:\n%s", res.stdout, run.stdout)
	}
	for _, m := range []string{"codex", "claude-opus"} {
		if v := readMeta(t, filepath.Join(outDir, m))["collectedAt"]; v != nil {
			t.Fatalf("a wait must not stamp member %s collected, got %v", m, v)
		}
	}
}

// A wait names the job it resolved to before it blocks, so whoever runs it
// sees at once which job it is waiting on.
func TestWaitNamesItsJobBeforeItBlocks(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	dispatch := exec.Command(binPath, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "1")...)
	dispatch.Env = e.build()
	if err := dispatch.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killDispatch(dispatch, outDir); dispatch.Wait() })
	waitForStatus(t, outDir, "running")

	wait := exec.Command(binPath, "wait", outDir)
	wait.Env = e.build()
	out, err := wait.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := wait.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wait.Process.Kill(); wait.Wait() })
	first := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "job: "+outDir+"\n" {
			t.Fatalf("first line = %q, want the job it waits on", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait printed nothing while it blocked")
	}
	if status := readMeta(t, outDir)["status"]; status != "running" {
		t.Fatalf("the job line arrived only after the turn ended (status %v)", status)
	}
}

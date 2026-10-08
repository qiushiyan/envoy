// Package integration exercises the built envoy binary end to end against
// the fake provider in fake-bin/, without billing a model. The suite covers
// the lifecycle contract: ok paths, failure recovery, timeouts, interruption,
// the terminal-envelope race, lock safety, and collection. This file is the
// harness every feature's file shares: the binary, the isolated environment,
// and the helpers that run and read a job.
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

// failureText is what a turn that did not deliver says about why: the
// result.md envoy wrote from its record, which a collect's error line repeats.
func failureText(t *testing.T, outDir string) string {
	t.Helper()
	return readFile(t, filepath.Join(outDir, "result.md"))
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

// stopTurns kills what a dispatch left running under outDir — each turn's
// provider group and the process running it — and waits until those
// processes are gone. A dispatch runs its turn in a process of its own, so
// killing the `envoy run` a test started stops only its waiting; a cleanup
// that left the rest would leak a runner and a fake provider into a deleted
// HOME.
func stopTurns(outDir string) {
	metas, _ := filepath.Glob(filepath.Join(outDir, "meta.json"))
	members, _ := filepath.Glob(filepath.Join(outDir, "*", "meta.json"))
	var runners []int
	for _, path := range append(metas, members...) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if pgid, ok := m["providerPgid"].(float64); ok && pgid > 0 {
			syscall.Kill(-int(pgid), syscall.SIGKILL)
		}
		if pid, ok := m["runnerPid"].(float64); ok && pid > 0 {
			syscall.Kill(int(pid), syscall.SIGKILL)
			runners = append(runners, int(pid))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range runners {
		for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// killDispatch stops a dispatch a test started and everything it left
// running.
func killDispatch(cmd *exec.Cmd, outDir string) {
	cmd.Process.Kill()
	stopTurns(outDir)
}

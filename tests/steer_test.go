// Steer contract: a supplement is never delivered into a live turn — no
// provider accepts one — so every steer answer is "not delivered" plus the one
// runnable command that carries the supplement to the session. These tests pin
// the wording per job state and the invariant that steer mutates nothing.
package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeSupplement(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "supplement.md")
	if err := os.WriteFile(p, []byte("also review the naming section\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustNotContain(t *testing.T, name, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Fatalf("%s must not contain %q, got:\n%s", name, sub, s)
		}
	}
}

// A live claude turn holds a session from the first moment, so steer can hand
// over the exact follow-up command — filled with the supplement, placeholder
// closed — while saying plainly that the in-flight turn will never see it.
func TestSteerLiveClaudeHandsOverFilledResume(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	supp := writeSupplement(t)

	cmd := exec.Command(binPath, turnArgs(prompt, outDir, "--provider", "claude", "--timeout-min", "5")...)
	cmd.Env = e.build()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Signal(syscall.SIGINT)
		cmd.Wait()
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("provider never accepted; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
		if data, err := os.ReadFile(filepath.Join(outDir, "meta.json")); err == nil &&
			strings.Contains(string(data), `"promptState": "accepted"`) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	res := runEnvoy(t, e, "steer", "--prompt-file", supp, outDir)
	if res.code != 0 {
		t.Fatalf("steer = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	session := readMeta(t, outDir)["sessionId"].(string)
	mustContain(t, "steer stdout", res.stdout,
		"steer: not delivered — this turn is still running, and claude takes no input into a turn in flight",
		"queue the supplement as a second turn",
		"job: "+outDir,
		"next: wait for this job to finish and read its result",
		"envoy turn --provider claude --resume "+session,
		"--timeout-min 5",
		"--prompt-file '"+supp+"'",
	)
	mustNotContain(t, "steer stdout", res.stdout, "<your-follow-up.md>")
}

// Before a fresh codex thread reports its id there is no session to continue,
// and steer must say so instead of inventing a command that names none.
func TestSteerLiveCodexWithoutSessionPointsAtCollect(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	supp := writeSupplement(t)

	cmd := exec.Command(binPath, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "5")...)
	cmd.Env = e.build()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Signal(syscall.SIGINT)
		cmd.Wait()
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("meta.json never appeared")
		}
		if data, err := os.ReadFile(filepath.Join(outDir, "meta.json")); err == nil &&
			strings.Contains(string(data), `"status": "running"`) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	res := runEnvoy(t, e, "steer", "--prompt-file", supp, outDir)
	if res.code != 0 {
		t.Fatalf("steer = %d\n%s", res.code, res.stderr)
	}
	mustContain(t, "steer stdout", res.stdout,
		"codex takes no input after dispatch",
		"no session id has been published yet",
		"envoy collect '"+outDir+"'",
	)
	mustNotContain(t, "steer stdout", res.stdout, "envoy turn")
}

// A finished ok turn: the supplement is the session's next prompt. An
// uncollected result is pointed at first — it may already cover the ask — and
// steer stamps nothing: reading a job's state never counts as delivery.
func TestSteerTerminalOkFillsResumeAndStampsNothing(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	supp := writeSupplement(t)

	if res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "codex", "--timeout-min", "5")...); res.code != 0 {
		t.Fatalf("turn = %d\n%s", res.code, res.stderr)
	}

	res := runEnvoy(t, e, "steer", "--prompt-file", supp, outDir)
	if res.code != 0 {
		t.Fatalf("steer = %d\n%s", res.code, res.stderr)
	}
	mustContain(t, "steer stdout", res.stdout,
		"steer: not delivered — this job is already terminal (status ok)",
		"read the result first",
		"envoy collect '"+outDir+"'",
		"envoy turn --provider codex --resume fake-session-id",
		"--prompt-file '"+supp+"'",
	)
	if meta := readMeta(t, outDir); meta["collectedAt"] != nil {
		t.Fatalf("steer must not stamp collectedAt, got %v", meta["collectedAt"])
	}

	// Once the result has been delivered, the collect-first nudge disappears.
	if res := runEnvoy(t, e, "collect", outDir); res.code != 0 {
		t.Fatalf("collect = %d\n%s", res.code, res.stderr)
	}
	res = runEnvoy(t, e, "steer", "--prompt-file", supp, outDir)
	mustContain(t, "steer after collect", res.stdout, "next: send the supplement as the same session's follow-up prompt")
	mustNotContain(t, "steer after collect", res.stdout, "read the result first")
}

// A non-ok terminal turn licenses nothing by itself: whether its session may
// continue is the job's own recovery decision, so steer routes through collect
// and never prints a resume of its own.
func TestSteerTerminalFailedRoutesThroughCollect(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "partial-failure")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	supp := writeSupplement(t)

	if res := runEnvoy(t, e, turnArgs(prompt, outDir, "--provider", "claude", "--timeout-min", "5")...); res.code != 1 {
		t.Fatalf("turn = %d, want 1\n%s", res.code, res.stderr)
	}

	res := runEnvoy(t, e, "steer", "--prompt-file", supp, outDir)
	if res.code != 0 {
		t.Fatalf("steer = %d\n%s", res.code, res.stderr)
	}
	mustContain(t, "steer stdout", res.stdout,
		"steer: not delivered — this job is already terminal (status failed)",
		"its session may continue is that job's own recovery decision",
		"envoy collect '"+outDir+"'",
	)
	mustNotContain(t, "steer stdout", res.stdout, "envoy turn")
}

// A job whose meta says running while its runner is gone has nothing listening.
// Steer points at collect — which owns the stranded-turn diagnosis — and reads
// without reconciling: the job on disk must be byte-identical afterwards.
func TestSteerStaleRunningJobMutatesNothing(t *testing.T) {
	e := newEnv(t)
	outDir := filepath.Join(t.TempDir(), "20260728-140000-delegate")
	os.MkdirAll(outDir, 0o755)
	os.WriteFile(filepath.Join(outDir, "prompt.md"), []byte("x"), 0o644)
	meta := map[string]any{
		"schemaVersion": 5, "status": "running", "provider": "codex",
		"promptState": "accepted", "sessionId": "dead-session",
		"runnerPid": 4194304, "providerPid": 4194304, "providerPgid": 4194304,
		"timeoutMin": 180.0, "nextAction": "wait", "resultKind": "none",
		"collectedAt": nil,
	}
	data, _ := json.Marshal(meta)
	metaPath := filepath.Join(outDir, "meta.json")
	os.WriteFile(metaPath, data, 0o644)
	supp := writeSupplement(t)

	res := runEnvoy(t, e, "steer", "--prompt-file", supp, outDir)
	if res.code != 0 {
		t.Fatalf("steer = %d\n%s", res.code, res.stderr)
	}
	mustContain(t, "steer stdout", res.stdout,
		"runner process is gone (abandoned)",
		"envoy collect '"+outDir+"'",
	)
	if got := readFile(t, metaPath); got != string(data) {
		t.Fatalf("steer rewrote meta.json:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(outDir, "result.md")); !os.IsNotExist(err) {
		t.Fatal("steer must not reconcile a stranded job into a result.md")
	}
}

// A fan-out directory holds no conversation of its own: steer hands each
// member its own runnable line, and the whole-set path is a new round.
func TestSteerGroupRedirectsPerMember(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "fan")
	prompt := writePrompt(t, t.TempDir())
	supp := writeSupplement(t)

	if res := runEnvoy(t, e, "fan", "--prompt-file", prompt, "--out-dir", outDir,
		"--with", "codex", "--with", "claude", "--timeout-min", "5"); res.code != 0 {
		t.Fatalf("fan = %d\n%s", res.code, res.stderr)
	}

	res := runEnvoy(t, e, "steer", "--prompt-file", supp, outDir)
	if res.code != 0 {
		t.Fatalf("steer = %d\n%s", res.code, res.stderr)
	}
	mustContain(t, "steer stdout", res.stdout,
		"steer: not delivered — this is a fan-out",
		"steer one member:",
		"envoy steer --prompt-file '"+supp+"' '"+filepath.Join(outDir, "codex")+"'",
		"envoy steer --prompt-file '"+supp+"' '"+filepath.Join(outDir, "claude")+"'",
		"envoy fan --resume-from '"+outDir+"' --timeout-min 5 --prompt-file '"+supp+"'",
	)
}

// Steer's own misuse answers: no supplement file, no job, a foreign meta.json.
func TestSteerUsageErrors(t *testing.T) {
	e := newEnv(t)
	supp := writeSupplement(t)

	res := runEnvoy(t, e, "steer")
	if res.code != 3 {
		t.Fatalf("bare steer = %d, want 3", res.code)
	}
	mustContain(t, "stderr", res.stderr, "--prompt-file <path> is required")

	res = runEnvoy(t, e, "steer", "--prompt-file", filepath.Join(t.TempDir(), "missing.md"))
	if res.code != 3 {
		t.Fatalf("missing supplement = %d, want 3", res.code)
	}
	mustContain(t, "stderr", res.stderr, "prompt file not found")

	empty := t.TempDir()
	res = runEnvoy(t, e, "steer", "--prompt-file", supp, empty)
	if res.code != 3 {
		t.Fatalf("empty dir = %d, want 3", res.code)
	}
	mustContain(t, "stderr", res.stderr, "meta.json not found", "out-dir printed at dispatch")

	foreign := t.TempDir()
	os.WriteFile(filepath.Join(foreign, "meta.json"), []byte(`{"commit":"abc"}`), 0o644)
	res = runEnvoy(t, e, "steer", "--prompt-file", supp, foreign)
	if res.code != 3 {
		t.Fatalf("foreign meta = %d, want 3", res.code)
	}
	mustContain(t, "stderr", res.stderr, "not a job this engine wrote")
}

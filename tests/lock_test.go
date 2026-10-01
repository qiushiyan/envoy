package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// One live turn per session: a conflict found before spawn refuses the turn,
// and one found mid-stream stops it.

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
	// envoy -h tells a caller to recognise a dispatch that ran nothing by this.
	mustNotContain(t, "stdout", res.stdout, "job:")
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
	if meta["promptStateEvidence"] != "codex thread.started with conflicting session lock" {
		t.Fatalf("evidence = %v", meta["promptStateEvidence"])
	}
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")),
		"# Turn infra", "another turn already holds")
	// The session may belong to the other turn, so no continuation of it is
	// offered; the next move is to collect the job that holds it.
	col := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "collect stdout", col.stdout, "session: fake-session-id", "next: Another turn holds this session id")
	mustNotContain(t, "collect stdout", col.stdout, "resume: envoy run")
}

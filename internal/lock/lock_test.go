package lock

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Redirect the state dir via HOME so tests never touch the real lock store.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestAcquireConflictRelease(t *testing.T) {
	isolate(t)

	h, err := Acquire("session-a", "/tmp/job1", "instance-1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// A second acquire against a live owner (this test process) must report
	// the live-owner conflict, never the stale-refusal one. The message text
	// is the contract: it is what the calling agent reads.
	_, err = Acquire("session-a", "/tmp/job2", "instance-2")
	conflict, ok := err.(*Conflict)
	if !ok {
		t.Fatalf("expected conflict, got %v", err)
	}
	if !strings.Contains(conflict.Message, "already has a live turn") ||
		!strings.Contains(conflict.Message, "/tmp/job1") {
		t.Fatalf("conflict message = %q", conflict.Message)
	}
	if strings.Contains(conflict.Message, "Automatic takeover is refused") {
		t.Fatal("live owner must not be reported as a stale refusal")
	}

	h.Release()
	if _, err := os.Stat(Path("session-a")); !os.IsNotExist(err) {
		t.Fatal("release must remove the lock file")
	}
	if h2, err := Acquire("session-a", "/tmp/job3", "instance-3"); err != nil {
		t.Fatalf("reacquire after release: %v", err)
	} else {
		h2.Release()
	}
}

func TestStaleLockIsNeverReclaimed(t *testing.T) {
	isolate(t)

	// A dead-owner lock: pid 4194304 exceeds every real pid space.
	p := Path("session-b")
	os.MkdirAll(strings.TrimSuffix(p, "/session-b.lock"), 0o755)
	payload, _ := json.Marshal(map[string]any{
		"pid": 4194304, "runnerInstanceId": "gone", "outDir": "/tmp/dead-job",
		"startedAt": "2026-01-01T00:00:00.000Z",
	})
	if err := os.WriteFile(p, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Acquire("session-b", "/tmp/job", "instance-1")
	conflict, ok := err.(*Conflict)
	if !ok {
		t.Fatalf("dead owner must be a conflict, got %v", err)
	}
	if !strings.Contains(conflict.Message, "Automatic takeover is refused") ||
		!strings.Contains(conflict.Message, "envoy collect '/tmp/dead-job'") {
		t.Fatalf("stale message must refuse takeover and point at the job: %q", conflict.Message)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("the stale lock must still exist — never auto-reclaimed")
	}
}

func TestReleaseRefusesForeignLock(t *testing.T) {
	isolate(t)

	h, err := Acquire("session-c", "/tmp/job", "instance-1")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate manual cleanup and reacquisition by another runner.
	os.Remove(Path("session-c"))
	h2, err := Acquire("session-c", "/tmp/other", "instance-2")
	if err != nil {
		t.Fatal(err)
	}

	h.Release() // stale handle must not remove instance-2's lock
	if _, err := os.Stat(Path("session-c")); err != nil {
		t.Fatal("a foreign lock must survive a stale handle's release")
	}
	h2.Release()
}

func TestSessionIDSanitizedInPath(t *testing.T) {
	isolate(t)
	p := Path("weird id/../with:stuff")
	if strings.ContainsAny(p[strings.LastIndex(p, "/")+1:], " /:") {
		t.Fatalf("lock filename must be sanitized: %q", p)
	}
}

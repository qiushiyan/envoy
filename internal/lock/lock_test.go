package lock

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/qiushiyan/envoy/internal/proc"
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
	if !strings.Contains(conflict.Error(), "already has a live turn") ||
		!strings.Contains(conflict.Error(), "/tmp/job1") {
		t.Fatalf("conflict message = %q", conflict.Error())
	}
	if strings.Contains(conflict.Error(), "Automatic takeover is refused") {
		t.Fatal("live owner must not be reported as a stale refusal")
	}
	// The caller is an agent that waits and collects; a terminal to tail is
	// not a thing it has.
	if strings.Contains(conflict.Error(), "tail -f") || !strings.Contains(conflict.Error(), "envoy collect '/tmp/job1'") {
		t.Fatalf("a live conflict must point at the owning job's collect, not a watch command: %q", conflict.Error())
	}

	h.Release()
	if _, err := os.Stat(lockPath("session-a")); !os.IsNotExist(err) {
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
	p := lockPath("session-b")
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
	if !strings.Contains(conflict.Error(), "Automatic takeover is refused") ||
		!strings.Contains(conflict.Error(), "envoy collect '/tmp/dead-job'") {
		t.Fatalf("stale message must refuse takeover and point at the job: %q", conflict.Error())
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
	os.Remove(lockPath("session-c"))
	h2, err := Acquire("session-c", "/tmp/other", "instance-2")
	if err != nil {
		t.Fatal(err)
	}

	h.Release() // stale handle must not remove instance-2's lock
	if _, err := os.Stat(lockPath("session-c")); err != nil {
		t.Fatal("a foreign lock must survive a stale handle's release")
	}
	h2.Release()
}

func TestSessionIDSanitizedInPath(t *testing.T) {
	isolate(t)
	p := lockPath("weird id/../with:stuff")
	if strings.ContainsAny(p[strings.LastIndex(p, "/")+1:], " /:") {
		t.Fatalf("lock filename must be sanitized: %q", p)
	}
}

// A holder whose job records a claim on its directory is live exactly while
// that claim is held. Its PID answering proves nothing: after a crash or a
// reboot the number can belong to any process, and calling that holder live
// would send the caller to wait on a turn that is gone.
func TestAHolderThatClaimedItsJobIsLiveOnlyWhileTheClaimIsHeld(t *testing.T) {
	isolate(t)
	outDir := t.TempDir()
	// The holder's recorded PID is this test process: alive, but not the runner.
	if err := os.WriteFile(outDir+"/meta.json", []byte(`{"schemaVersion":10,"status":"running","runnerLock":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := Acquire("session-c", outDir, "instance-1")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	_, err = Acquire("session-c", "/tmp/other", "instance-2")
	conflict, ok := err.(*Conflict)
	if !ok {
		t.Fatalf("expected conflict, got %v", err)
	}
	if conflict.HolderLive {
		t.Fatalf("a holder whose claim is free must not read as live: %q", conflict.Error())
	}
	claim, err := proc.LockDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Release()
	_, err = Acquire("session-c", "/tmp/other", "instance-2")
	if conflict, ok := err.(*Conflict); !ok || !conflict.HolderLive {
		t.Fatalf("a holder whose claim is held must read as live, got %v", err)
	}
}

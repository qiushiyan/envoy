package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A claim reads as its holder's liveness: held while the holder keeps it, free
// once it lets go, and unknown for a directory that cannot be probed.
func TestDirLockLivenessFollowsTheClaim(t *testing.T) {
	dir := t.TempDir()
	if got := DirLockLiveness(dir); got != Gone {
		t.Fatalf("unclaimed dir = %v, want Gone", got)
	}
	claim, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := DirLockLiveness(dir); got != Live {
		t.Fatalf("claimed dir = %v, want Live", got)
	}
	claim.Release()
	if got := DirLockLiveness(dir); got != Gone {
		t.Fatalf("released dir = %v, want Gone", got)
	}
	if got := DirLockLiveness(filepath.Join(dir, "absent")); got != Unknown {
		t.Fatalf("missing dir = %v, want Unknown", got)
	}
}

// Waiting on a claim returns when it is released, not before.
func TestAwaitDirUnlockReturnsOnRelease(t *testing.T) {
	dir := t.TempDir()
	claim, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- AwaitDirUnlock(dir) }()
	select {
	case err := <-done:
		t.Fatalf("returned while the claim was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	claim.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not return after the claim was released")
	}
}

// A runner's liveness is read from its claim when it recorded one, whatever
// its records say of the work; without one, records that establish the work
// finished make a PID that answers no evidence, and only records that leave
// it unfinished fall back on the PID.
func TestRunnerLivenessReadsTheClaimThenCompletionThenThePID(t *testing.T) {
	dir := t.TempDir()
	alive := os.Getpid() // stands in for a PID another process may now hold
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	gone := exited.Process.Pid
	read := func(name string, r Runner, want Liveness) {
		t.Helper()
		if got := RunnerLiveness(r); got != want {
			t.Errorf("%s: RunnerLiveness(%+v) = %v, want %v", name, r, got, want)
		}
	}
	read("claimed, claim free, its PID answering", Runner{Dir: dir, Pid: alive, Claimed: true}, Gone)
	read("unclaimed, work finished, its PID answering", Runner{Dir: dir, Pid: alive, Finished: true}, Gone)
	read("unclaimed, work unfinished, its PID answering", Runner{Dir: dir, Pid: alive}, Live)
	read("unclaimed, work unfinished, its PID gone", Runner{Dir: dir, Pid: gone}, Gone)
	read("unclaimed, work unfinished, no PID recorded", Runner{Dir: dir}, Unknown)

	claim, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Release()
	read("claimed, claim held, work finished", Runner{Dir: dir, Claimed: true, Finished: true}, Live)
}

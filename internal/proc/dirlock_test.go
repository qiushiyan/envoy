package proc

import (
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

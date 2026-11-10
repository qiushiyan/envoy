package proc

import (
	"errors"
	"os"
	"syscall"
)

// DirLock is a process's claim on a directory: an exclusive flock(2) held
// for as long as the holder keeps it. The kernel releases it when the holder
// exits, however it exits, so — unlike a recorded PID, which a later process
// can reuse — a lock still held proves its holder is alive, and a lock that
// is free proves it is gone.
//
// The descriptor is close-on-exec, so a child the holder spawns never
// inherits the claim and cannot keep it alive past the holder.
type DirLock struct {
	f *os.File
}

// LockDir claims dir, waiting while a prober briefly holds it shared.
func LockDir(dir string) (*DirLock, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := flock(f, syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &DirLock{f: f}, nil
}

// Release gives the claim up. A nil lock is a claim never taken.
func (l *DirLock) Release() {
	if l != nil {
		l.f.Close()
	}
}

// DirLockLiveness reports whether a holder's claim on dir is held: Live while
// it is, Gone once it is free, Unknown when the directory cannot be probed.
func DirLockLiveness(dir string) Liveness {
	f, err := os.Open(dir)
	if err != nil {
		return Unknown
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	switch {
	case err == nil:
		return Gone
	case errors.Is(err, syscall.EWOULDBLOCK):
		return Live
	}
	return Unknown
}

// Runner is what a job's records say about the process supervising it: the
// directory it claims, its PID, whether it recorded holding that claim, and
// whether the records establish that the work it supervised is finished.
// Finished is established, never assumed: a record that is missing, or one
// whose meaning cannot be read, leaves it false.
type Runner struct {
	Dir      string
	Pid      int
	Claimed  bool
	Finished bool
}

// RunnerLiveness is the single reading of whether the process supervising a
// job is alive. A runner that recorded its claim on its directory is alive
// exactly while the claim is held — past its final record, until it returns —
// which a reused PID cannot fake. One that did not (an older record, or a
// claim that could not be taken) is known only by its PID, and a PID is
// evidence only while the work it supervised is unfinished: once the records
// establish that work finished, the number may since belong to any process,
// so the runner reads as gone. Records that leave it unfinished leave the PID
// the only evidence there is, which no reading can tell from a reuse.
func RunnerLiveness(r Runner) Liveness {
	switch {
	case r.Claimed:
		return DirLockLiveness(r.Dir)
	case r.Finished:
		return Gone
	}
	return PidLiveness(r.Pid)
}

// AwaitDirUnlock blocks until no claim on dir is held.
func AwaitDirUnlock(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return flock(f, syscall.LOCK_SH)
}

// flock takes a blocking lock, retrying the interruptions a signal delivered
// to the process causes.
func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

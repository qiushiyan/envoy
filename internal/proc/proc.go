// Package proc probes and signals processes and process groups.
//
// Liveness is tri-state because PID probes are best-effort: operating systems
// reuse IDs, and a permission error still proves *something* owns the PID.
package proc

import (
	"errors"
	"syscall"
)

// Liveness is the answer a PID probe can actually give.
type Liveness int

const (
	Unknown Liveness = iota // probe failed for a reason other than ESRCH/EPERM
	Live                    // signal 0 delivered, or EPERM (a live process we can't signal)
	Gone                    // ESRCH
)

func classify(err error) Liveness {
	switch {
	case err == nil:
		return Live
	case errors.Is(err, syscall.EPERM):
		return Live
	case errors.Is(err, syscall.ESRCH):
		return Gone
	default:
		return Unknown
	}
}

// PidLiveness probes a single process with signal 0.
func PidLiveness(pid int) Liveness {
	if pid <= 0 {
		return Unknown
	}
	return classify(syscall.Kill(pid, 0))
}

// GroupLiveness probes a process group with signal 0.
func GroupLiveness(pgid int) Liveness {
	if pgid <= 0 {
		return Unknown
	}
	return classify(syscall.Kill(-pgid, 0))
}

// Alive reports the two-state view used by session locks, where EPERM counts
// as alive and any probe failure other than ESRCH is treated as gone.
func Alive(pid int) bool {
	return PidLiveness(pid) == Live
}

// SignalGroup sends sig to the whole process group. A vanished group (ESRCH)
// is success: the goal was for it to be gone.
func SignalGroup(pgid int, sig syscall.Signal) error {
	err := syscall.Kill(-pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

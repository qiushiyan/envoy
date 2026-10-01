// Package proc probes and signals processes and process groups.
//
// Liveness is tri-state because PID probes are best-effort: operating systems
// reuse IDs, and a permission error still proves *something* owns the PID.
package proc

import (
	"errors"
	"os"
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

var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP:  "SIGHUP",
	syscall.SIGINT:  "SIGINT",
	syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGABRT: "SIGABRT",
	syscall.SIGKILL: "SIGKILL",
	syscall.SIGBUS:  "SIGBUS",
	syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGPIPE: "SIGPIPE",
	syscall.SIGTERM: "SIGTERM",
}

// SignalName spells a signal the way records and prose name it (SIGTERM),
// falling back to the platform's description for one without a fixed name.
func SignalName(sig os.Signal) string {
	if s, ok := sig.(syscall.Signal); ok {
		if name, ok := signalNames[s]; ok {
			return name
		}
	}
	return sig.String()
}

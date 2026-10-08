// Package detach runs a dispatch in a process the caller does not own.
//
// The `envoy run` a caller starts becomes a waiter. It starts the dispatch
// through a short-lived intermediate that leads a new session and exits at
// once, so the process that runs the turn is neither in the caller's process
// group nor below it in the process tree. The waiter relays that process's
// stdout and stderr, forwards an interrupt to it, and exits with its exit
// code, so a caller that keeps waiting sees what it always saw. A harness
// that tears the caller down — Claude Code ends a session, and stops a
// background task, by signalling the task's process group and every process
// below it — stops only the waiting; the turn runs on, and `envoy wait`
// waits for it again.
//
// The detached process runs the dispatch exactly as an attached one would;
// nothing here reads a flag, a record or a word of prose about the job.
package detach

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// The hidden argv entries of the two processes a waiter starts. They are
// argv, never environment: a provider inherits the environment, and one that
// dispatches envoy itself would otherwise take a hidden path.
const (
	SpawnEntry    = "__envoy-spawn"
	DetachedEntry = "__envoy-detached"
)

// controlFd is the descriptor the detached process reports on: its PID
// first, its exit code last.
const controlFd = 3

// Dispatch is the waiter: it runs the dispatch with runArgs in a detached
// process and returns that dispatch's exit code. A dispatch whose process
// cannot be started dispatches nothing: there is one way a turn runs.
func Dispatch(runArgs []string, stdout, stderr io.Writer) int {
	jobRef := ""
	if len(runArgs) > 0 && !strings.HasPrefix(runArgs[0], "-") {
		jobRef = runArgs[0]
	}
	notStarted := func(err error) int {
		fmt.Fprintln(stderr, prose.DetachFailed(err))
		return job.ExitInfra
	}
	exe, err := os.Executable()
	if err != nil {
		return notStarted(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return notStarted(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)
		return notStarted(err)
	}
	ctlR, ctlW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW, errR, errW)
		return notStarted(err)
	}

	// Signals are ours from before the start, so none can take the default
	// path between the detached process starting and the waiter forwarding.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	cmd := exec.Command(exe, append([]string{SpawnEntry}, runArgs...)...)
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.ExtraFiles = []*os.File{ctlW}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	startErr := cmd.Start()
	// Only the detached side may hold the write ends, so they close here
	// whether or not it started: end of stream then means it has exited.
	closeAll(outW, errW, ctlW)
	if startErr != nil {
		closeAll(outR, errR, ctlR)
		return notStarted(startErr)
	}
	go cmd.Wait() // the intermediate exits at once; reap it

	var relays sync.WaitGroup
	for _, pipe := range []struct {
		from *os.File
		to   io.Writer
	}{{outR, stdout}, {errR, stderr}} {
		relays.Add(1)
		go func() {
			defer relays.Done()
			io.Copy(pipe.to, pipe.from)
			pipe.from.Close()
		}()
	}
	relayed := make(chan struct{})
	go func() { relays.Wait(); close(relayed) }()
	reports := readReports(ctlR)

	pid, code, interrupts := 0, -1, 0
	for reports != nil || relayed != nil {
		select {
		case r, ok := <-reports:
			switch {
			case !ok:
				// The detached process is gone: its PID is no longer its.
				reports, pid = nil, 0
			case r.pid > 0:
				pid = r.pid
				for ; interrupts > 0; interrupts-- {
					syscall.Kill(pid, syscall.SIGINT)
				}
			default:
				code = r.code
			}
		case <-relayed:
			relayed = nil
		case sig := <-sigCh:
			if sig == syscall.SIGINT {
				// An interrupt is a decision about the turn, so it reaches
				// the turn; the runner stops its provider tree, and a
				// second one kills it at once, as it always has.
				if pid > 0 {
					syscall.Kill(pid, syscall.SIGINT)
				} else if reports != nil {
					interrupts++
				}
				continue
			}
			// SIGTERM and SIGHUP are what a harness or a terminal sends
			// when the caller is going away, and the only way it stops a
			// background task: they stop the waiting, never the turn.
			fmt.Fprintln(stderr, prose.StoppedWaiting(proc.SignalName(sig), jobRef))
			signal.Reset(sig)
			s := sig.(syscall.Signal)
			syscall.Kill(os.Getpid(), s)
			return 128 + int(s)
		}
	}
	if code < 0 {
		fmt.Fprintln(stderr, prose.DispatchLost())
		return job.ExitInfra
	}
	return code
}

// report is one line the detached process wrote on its control descriptor.
type report struct {
	pid, code int
}

func readReports(r *os.File) <-chan report {
	ch := make(chan report, 2)
	go func() {
		defer close(ch)
		defer r.Close()
		lines := bufio.NewScanner(r)
		for lines.Scan() {
			kind, value, _ := strings.Cut(lines.Text(), " ")
			n, err := strconv.Atoi(value)
			if err != nil {
				continue
			}
			switch kind {
			case "pid":
				ch <- report{pid: n}
			case "exit":
				ch <- report{code: n}
			}
		}
	}()
	return ch
}

// Spawn is the intermediate. It leads the session the waiter started it in,
// starts the detached process inside that session and exits, which hands the
// detached process to init: no longer below the caller in the process tree,
// and never a session leader, so it can never acquire a controlling terminal.
func Spawn(runArgs []string) int {
	ctl := os.NewFile(controlFd, "control")
	// A detached process that never starts reports nothing, so its exit code
	// is reported here: nothing was dispatched.
	notStarted := func(err error) int {
		fmt.Fprintln(os.Stderr, prose.DetachFailed(err))
		fmt.Fprintf(ctl, "exit %d\n", job.ExitInfra)
		return job.ExitInfra
	}
	exe, err := os.Executable()
	if err != nil {
		return notStarted(err)
	}
	cmd := exec.Command(exe, append([]string{DetachedEntry}, runArgs...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{ctl}
	if err := cmd.Start(); err != nil {
		return notStarted(err)
	}
	return job.ExitOK
}

// Serve is the detached process: it reports its PID, runs the dispatch, and
// reports the dispatch's exit code.
//
// Its stdout and stderr are the waiter's relays, so a panic's trace reaches
// a caller that is still waiting. Once the waiter is gone a write to them
// fails instead of killing the process: SIGPIPE is caught, never ignored —
// an ignored signal would be inherited by the provider, a caught one is
// reset when the provider is exec'd. The control descriptor is marked
// close-on-exec before anything is spawned, so no provider or grandchild can
// hold it open past this process.
func Serve(dispatch func() int) int {
	syscall.CloseOnExec(controlFd)
	ctl := os.NewFile(controlFd, "control")
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	fmt.Fprintf(ctl, "pid %d\n", os.Getpid())
	code := dispatch()
	fmt.Fprintf(ctl, "exit %d\n", code)
	ctl.Close()
	return code
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		f.Close()
	}
}

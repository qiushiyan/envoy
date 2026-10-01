package runner

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"

	"github.com/qiushiyan/envoy/internal/job"

	"github.com/qiushiyan/envoy/internal/proc"
)

// commandPrefix is resolved for each turn, including continuations. It is
// argv, not shell code: quoting, expansion and escaping are not interpreted.
func commandPrefix(provider string) []string {
	words := strings.Fields(os.Getenv("ENVOY_" + strings.ToUpper(provider) + "_CMD"))
	if len(words) == 0 {
		return []string{provider}
	}
	return words
}

// childProcess wraps the provider CLI with raw pipe fds. The fds are passed
// directly (no exec-package copy goroutines), so wait() observes pure process
// exit — the moment a grandchild might still hold the pipes — and stream EOF
// is a separate, later observation. The runner needs both, separately.
type childProcess struct {
	cmd    *exec.Cmd
	pid    int
	stdin  *os.File
	stdout *os.File
	stderr *os.File
}

func withoutCallerIdentity(environ []string) []string {
	kept := environ[:0:0]
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(job.CallerEnvKeys, key) {
			kept = append(kept, kv)
		}
	}
	return kept
}

func spawn(name string, argv []string, cwd string, extraEnv []string) (*childProcess, error) {
	cmd := exec.Command(name, argv...)
	cmd.Dir = cwd
	// The child is a session of its own. Left in its environment, the
	// dispatcher's identity would make every job the child dispatches through
	// envoy answer to the dispatcher's names.
	cmd.Env = append(withoutCallerIdentity(os.Environ()), extraEnv...)
	// The child leads its own process group so timeout and cancellation can
	// stop provider grandchildren too, rather than only the CLI parent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW, errR, errW} {
			f.Close()
		}
		return nil, err
	}
	// Parent copies of the child-side ends must close, or EOF never arrives.
	inR.Close()
	outW.Close()
	errW.Close()

	return &childProcess{
		cmd:    cmd,
		pid:    cmd.Process.Pid,
		stdin:  inW,
		stdout: outR,
		stderr: errR,
	}, nil
}

// pump forwards read chunks to the event loop and closes ch at EOF.
func (c *childProcess) pump(f *os.File, ch chan []byte) {
	defer close(ch)
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			ch <- chunk
		}
		if err != nil {
			return
		}
	}
}

// wait blocks until the process exits and reports code/signal the way the
// meta schema records them: a signal death has a signal name and a nil code.
func (c *childProcess) wait() exitResult {
	c.cmd.Wait() // the error restates ProcessState, which is read below
	state := c.cmd.ProcessState
	if state == nil {
		return exitResult{}
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		name := proc.SignalName(ws.Signal())
		return exitResult{signal: &name}
	}
	code := state.ExitCode()
	return exitResult{code: &code}
}

// signalTree signals the whole process group, falling back to the direct
// child when the group probe fails for a reason other than "already gone".
func (r *run) signalTree(sig syscall.Signal) {
	if r.child == nil || r.done {
		return
	}
	err := syscall.Kill(-r.child.pid, sig)
	if err == nil || err == syscall.ESRCH {
		return
	}
	syscall.Kill(r.child.pid, sig)
}

func (r *run) groupAlive() bool {
	if r.child == nil {
		return false
	}
	return proc.GroupLiveness(r.child.pid) == proc.Live
}

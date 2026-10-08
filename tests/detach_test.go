package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startLeading starts `envoy run` as the leader of a process group of its
// own, the way a harness starts a background task, so the test can stop it
// the way the harness does: by signalling that group.
func startLeading(t *testing.T, e *env, dir string, args ...string) (*exec.Cmd, *strings.Builder) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env, cmd.Dir = e.build(), dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, &stderr
}

// tearDown stops a dispatch the way Claude Code ends a session or stops a
// background task: the signal to the task's process group and to every
// process below it, then SIGKILL to the same set. The set is taken first, as
// a harness walking the tree takes it, so a turn still below the dispatch —
// one never handed to init — is in it.
func tearDown(t *testing.T, cmd *exec.Cmd, sig syscall.Signal) {
	t.Helper()
	tree := descendants(t, cmd.Process.Pid)
	syscall.Kill(-cmd.Process.Pid, sig)
	for _, pid := range tree {
		syscall.Kill(pid, sig)
	}
	time.Sleep(100 * time.Millisecond)
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	for _, pid := range tree {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	cmd.Wait()
}

// descendants lists every process below root, from one snapshot of the
// process table.
func descendants(t *testing.T, root int) []int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		t.Fatal(err)
	}
	children := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		var pid, ppid int
		if _, err := fmt.Sscan(line, &pid, &ppid); err == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var found []int
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		for _, child := range children[queue[0]] {
			found = append(found, child)
			queue = append(queue, child)
		}
	}
	return found
}

// The caller's environment going away stops only the waiting, whichever of
// its signals it sends. The turn its dispatch started — a write turn here,
// the kind most costly to lose halfway — runs on to its own end, records what
// happened, and a wait tells the caller when it ended.
func TestATurnOutlivesItsCaller(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		name := map[syscall.Signal]string{syscall.SIGTERM: "SIGTERM", syscall.SIGHUP: "SIGHUP"}[sig]
		t.Run(name, func(t *testing.T) {
			e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "delayed-success").set("ENVOY_FAKE_DELAY_MS", "1500")
			outDir := filepath.Join(t.TempDir(), "job")
			prompt := writePrompt(t, t.TempDir())
			cmd, stderr := startLeading(t, e, "", runArgs(prompt, outDir, "--with", "codex", "--allow-write", "--timeout-min", "1")...)
			t.Cleanup(func() { stopTurns(outDir) })
			waitForStatus(t, outDir, "running")

			tearDown(t, cmd, sig)
			if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() {
				t.Fatalf("the dispatch must end by the signal that stopped it, got %v", cmd.ProcessState)
			}
			mustContain(t, "dispatch stderr", stderr.String(),
				"received "+name+", so this command stopped waiting; the turn keeps running",
				"envoy wait '"+outDir+"'",
			)

			res := runEnvoy(t, e, "wait", outDir)
			if res.code != 0 {
				t.Fatalf("wait exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
			}
			mustContain(t, "wait stdout", res.stdout, "status: ok")
			meta := readMeta(t, outDir)
			if meta["status"] != "ok" || meta["interruptionSignal"] != nil || meta["allowWrite"] != true {
				t.Fatalf("turn = status %v, interruption %v, write %v; want an ok write turn, uninterrupted",
					meta["status"], meta["interruptionSignal"], meta["allowWrite"])
			}
			if meta["collectedAt"] != nil {
				t.Fatalf("nothing collected it, yet collectedAt = %v", meta["collectedAt"])
			}
		})
	}
}

// A fan-out runs its members in one process of its own, so it outlives its
// caller as a whole.
func TestAFanOutOutlivesItsCaller(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_DELAY_MS", "1500").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "codex-session").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "claude-session")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	cmd, _ := startLeading(t, e, "", runArgs(prompt, outDir, "--with", "codex", "--with", "claude:opus", "--timeout-min", "1")...)
	t.Cleanup(func() { stopTurns(outDir) })
	waitForStatus(t, filepath.Join(outDir, "codex"), "running")
	waitForStatus(t, filepath.Join(outDir, "claude-opus"), "running")

	tearDown(t, cmd, syscall.SIGTERM)
	res := runEnvoy(t, e, "wait", outDir)
	if res.code != 0 {
		t.Fatalf("wait exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "wait stdout", res.stdout, "member codex: ok", "member claude-opus: ok")
}

// With the dispatch's own process gone, collect is where a running turn's
// stop is found, and that command stops it the way Ctrl-C on the dispatch
// would have.
func TestTheStopCommandStopsADetachedTurn(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	cmd, _ := startLeading(t, e, "", runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "1")...)
	t.Cleanup(func() { stopTurns(outDir) })
	waitForStatus(t, outDir, "running")
	tearDown(t, cmd, syscall.SIGTERM)

	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	runner := int(readMeta(t, outDir)["runnerPid"].(float64))
	stop := fmt.Sprintf("stop: kill -INT %d", runner)
	mustContain(t, "collect stdout", status.stdout, stop, "envoy wait '"+outDir+"'")
	full := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "plain collect", full.stdout, stop)

	if err := exec.Command("sh", "-c", strings.TrimPrefix(stop, "stop: ")).Run(); err != nil {
		t.Fatal(err)
	}
	res := runEnvoy(t, e, "wait", outDir)
	if res.code != 5 {
		t.Fatalf("wait exit = %d, want 5\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	if meta := readMeta(t, outDir); meta["status"] != "interrupted" || meta["interruptionSignal"] != "SIGINT" {
		t.Fatalf("turn = status %v, interruption %v; want interrupted by SIGINT", meta["status"], meta["interruptionSignal"])
	}
}

// A turn still running is listed to a caller that may have lost its
// dispatch — its own, or either side without an identity, which share every
// name — with the wait that tells it when the turn ends, and to no other
// caller: another session's healthy turn is not theirs to wait on. A wait by
// name means the caller's own job, and says so when it fell back to another's.
func TestPendingListsALiveTurnOnlyToItsCaller(t *testing.T) {
	base := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	mine, theirs, anonymous := base.as("session-a", "sess-a"), base.as("session-b", "sess-b"), base.as("", "sess-anon")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	// dispatchDetached starts a long turn as e and stops its dispatch the way
	// a harness does, returning the job directory the turn still runs in.
	dispatchDetached := func(e *env, name string) string {
		cmd, _ := startLeading(t, e, project, "run", name, "--prompt-file", prompt, "--with", "codex", "--timeout-min", "1")
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			r := runEnvoyIn(t, e, project, "collect", "--status-only", name)
			if dir, ok := strings.CutPrefix(strings.SplitN(r.stdout, "\n", 2)[0], "job: "); ok && strings.Contains(r.stdout, "status: running") {
				t.Cleanup(func() { stopTurns(dir) })
				tearDown(t, cmd, syscall.SIGTERM)
				return dir
			}
			time.Sleep(20 * time.Millisecond)
		}
		killDispatch(cmd, "")
		t.Fatalf("%s never showed as running", name)
		return ""
	}
	outDir := dispatchDetached(mine, "review-r1")
	anonDir := dispatchDetached(anonymous, "scratch")

	runner := int(readMeta(t, outDir)["runnerPid"].(float64))
	listed := runEnvoyIn(t, mine, project, "pending")
	mustContain(t, "pending (its caller)", listed.stdout, "[running] "+outDir, "envoy wait '"+outDir+"'",
		fmt.Sprintf("stop: kill -INT %d", runner), "[running] "+anonDir)
	other := runEnvoyIn(t, theirs, project, "pending")
	mustNotContain(t, "pending (another caller)", other.stdout, "[running] "+outDir)
	mustContain(t, "pending (another caller)", other.stdout, "[running] "+anonDir)
	none := runEnvoyIn(t, anonymous, project, "pending")
	mustContain(t, "pending (no identity)", none.stdout, "[running] "+outDir, "[running] "+anonDir)

	if err := syscall.Kill(runner, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	own := runEnvoyIn(t, mine, project, "wait", "review-r1")
	if own.code != 5 || !strings.Contains(own.stdout, "job: "+outDir) || strings.Contains(own.stdout, "note:") {
		t.Fatalf("its caller's wait by name: exit %d\n%s%s", own.code, own.stdout, own.stderr)
	}
	borrowed := runEnvoyIn(t, theirs, project, "wait", "review-r1")
	mustContain(t, "another caller's wait by name", borrowed.stdout,
		"job: "+outDir, "note: this session has dispatched no job named review-r1")
}

// Refusals before anything runs reach the caller through the waiter, with
// their exit code.
func TestARefusalReachesTheCallerThroughTheWaiter(t *testing.T) {
	res := runEnvoy(t, newEnv(t), "run", "job", "--with", "codex")
	if res.code != 3 || !strings.Contains(res.stderr, "usage error: --with codex has no prompt") {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
}

// The provider starts with only its own stdin, stdout and stderr. A
// descriptor of the dispatch's — the report it makes to its waiter, its
// claim on the job dir — held by a provider would outlive the dispatch in
// any grandchild the provider leaves behind, and the waiter would wait on
// that grandchild instead of on the turn.
func TestTheProviderInheritsNoDescriptorOfTheDispatch(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "open-fds")
	launcher := filepath.Join(dir, "fd-probe")
	script := "#!/bin/sh\nfor fd in 3 4 5 6 7 8 9; do [ -e /dev/fd/$fd ] && echo $fd >> " + record + "; done\ntouch " + record + "\nexec \"$@\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t).set("ENVOY_CODEX_CMD", launcher+" codex")
	outDir := filepath.Join(t.TempDir(), "job")
	res := runEnvoy(t, e, runArgs(writePrompt(t, t.TempDir()), outDir, "--with", "codex", "--timeout-min", "1")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	if open := readFile(t, record); open != "" {
		t.Fatalf("the provider inherited descriptors beyond stdio: %q", open)
	}
}

// A fan-out whose dispatch was stopped is found and stopped as one job: every
// member runs in one process, so pending lists the fan-out once, collect
// offers one stop above the members, and that stop interrupts every member. A
// member collected on its own offers the same stop and says it reaches every
// member, since it cannot stop that member alone.
func TestADetachedFanOutIsFoundAndStoppedAsOneJob(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "codex-session").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "claude-session")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	cmd, _ := startLeading(t, e, project, "run", "pair", "--prompt-file", prompt, "--with", "codex", "--with", "claude:opus", "--timeout-min", "1")
	var outDir string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r := runEnvoyIn(t, e, project, "collect", "--status-only", "pair")
		if dir, ok := strings.CutPrefix(strings.SplitN(r.stdout, "\n", 2)[0], "fan-out: "); ok && strings.Contains(r.stdout, "members: codex running · claude-opus running") {
			outDir = dir
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if outDir == "" {
		killDispatch(cmd, "")
		t.Fatal("the fan-out never showed both members running")
	}
	t.Cleanup(func() { stopTurns(outDir) })
	tearDown(t, cmd, syscall.SIGTERM)

	var group map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(outDir, "group.json"))), &group); err != nil {
		t.Fatal(err)
	}
	stop := fmt.Sprintf("stop: kill -INT %d", int(group["runnerPid"].(float64)))

	pending := runEnvoyIn(t, e, project, "pending")
	mustContain(t, "pending", pending.stdout, "[running] "+outDir, "envoy wait '"+outDir+"'", stop)
	whole := runEnvoyIn(t, e, project, "collect", "--status-only", "pair")
	mustContain(t, "fan-out collect", whole.stdout, stop, "envoy wait '"+outDir+"'")
	if n := strings.Count(whole.stdout, "stop: "); n != 1 {
		t.Fatalf("the fan-out's one stop must print once, above the members; printed %d times:\n%s", n, whole.stdout)
	}
	member := runEnvoyIn(t, e, project, "collect", "--status-only", filepath.Join(outDir, "codex"))
	mustContain(t, "member collect", member.stdout, stop, "interrupts every member of the fan-out '"+outDir+"'")

	if err := exec.Command("sh", "-c", strings.TrimPrefix(stop, "stop: ")).Run(); err != nil {
		t.Fatal(err)
	}
	res := runEnvoyIn(t, e, project, "wait", "pair")
	if res.code != 5 {
		t.Fatalf("wait exit = %d, want 5\n%s%s", res.code, res.stdout, res.stderr)
	}
	for _, m := range []string{"codex", "claude-opus"} {
		if meta := readMeta(t, filepath.Join(outDir, m)); meta["status"] != "interrupted" || meta["interruptionSignal"] != "SIGINT" {
			t.Fatalf("member %s = status %v, interruption %v; want interrupted by SIGINT", m, meta["status"], meta["interruptionSignal"])
		}
	}
}

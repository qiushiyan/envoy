package collect

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// writeClaimedTurn writes a turn record whose runner recorded its claim. The
// recorded runner PID is this test process — alive, and not the runner — so
// only the claim can say whether the runner lives; the provider is gone.
func writeClaimedTurn(t *testing.T, dir, status string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gone := deadPid(t)
	meta := fmt.Sprintf(`{"schemaVersion":10,"status":%q,"provider":"codex","promptState":"accepted","timeoutMin":5,`+
		`"sessionId":"s-%s","collectedAt":null,"runnerPid":%d,"runnerLock":true,"providerPid":%d,"providerPgid":%d}`,
		status, filepath.Base(dir), os.Getpid(), gone, gone)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeClaimedGroup writes a fan-out manifest whose process recorded its claim
// and whose recorded PID, this test process, is not that process.
func writeClaimedGroup(t *testing.T, dir string, members ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, len(members))
	for i, m := range members {
		quoted[i] = fmt.Sprintf("%q", m)
		os.MkdirAll(filepath.Join(dir, m), 0o755)
	}
	group := fmt.Sprintf(`{"schemaVersion":2,"startedAt":"2026-09-21T00:00:00.000Z","cwd":"/tmp","timeoutMin":5,"gitBaseline":null,`+
		`"members":[%s],"runnerPid":%d,"runnerLock":true}`, strings.Join(quoted, ","), os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "group.json"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitOn(t *testing.T, dir string) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := Wait(dir, "", &out, &errOut)
	return code, out.String() + errOut.String()
}

// A wait on a turn whose runner is gone without a final record returns at
// once, says the ending is not recorded, and sends the caller to collect,
// which reconciles; the wait itself leaves the record as it found it.
func TestWaitReportsARunnerThatLeftNoFinalRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	writeClaimedTurn(t, dir, "running")
	code, out := waitOn(t, dir)
	if code != job.ExitInfra {
		t.Fatalf("exit = %d, want %d\n%s", code, job.ExitInfra, out)
	}
	if !strings.Contains(out, "left no final record") || !strings.Contains(out, "envoy collect '"+dir+"'") {
		t.Fatalf("block must say no final record was left and name the collect:\n%s", out)
	}
	if _, m, _ := job.ReadRecord(dir); m.Status != job.StatusRunning {
		t.Fatalf("a wait must not reconcile the record, status = %q", m.Status)
	}
}

// A fan-out whose process is gone ends cleanly only when every member left a
// final record, or none because it never started. A member still recorded as
// running, or one whose record cannot be read, did not end the way the
// dispatch's ending block describes — that block would call it never
// dispatched, or claim no member returned a result beside an ok one — so the
// wait reports the ending as unrecorded and sends the caller to collect.
func TestWaitOnAFanOutWhoseMembersLeftNoFinalRecord(t *testing.T) {
	for _, c := range []struct {
		name  string
		build func(dir string)
	}{
		{"a member still recorded as running", func(dir string) {
			writeClaimedTurn(t, filepath.Join(dir, "claude"), "running")
		}},
		{"a member whose record cannot be read", func(dir string) {
			os.WriteFile(filepath.Join(dir, "claude", "meta.json"), []byte("{"), 0o644)
		}},
	} {
		dir := filepath.Join(t.TempDir(), "job")
		writeClaimedGroup(t, dir, "codex", "claude")
		writeClaimedTurn(t, filepath.Join(dir, "codex"), "ok")
		c.build(dir)
		code, out := waitOn(t, dir)
		if code != job.ExitInfra {
			t.Errorf("%s: exit = %d, want %d\n%s", c.name, code, job.ExitInfra, out)
		}
		for _, wrong := range []string{"never dispatched", "no member returned a result", "status: running"} {
			if strings.Contains(out, wrong) {
				t.Errorf("%s: block must not say %q:\n%s", c.name, wrong, out)
			}
		}
		if !strings.Contains(out, "claude") || !strings.Contains(out, "envoy collect '"+dir+"'") {
			t.Errorf("%s: block must name the member and the collect:\n%s", c.name, out)
		}
	}
}

// A member that never wrote a record never started a provider — the runner
// writes its first record before it spawns one — so once the fan-out's
// process is gone it is reported as never dispatched, beside the members that
// ended.
func TestWaitOnAFanOutWithAMemberThatNeverStarted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	writeClaimedGroup(t, dir, "codex", "claude")
	writeClaimedTurn(t, filepath.Join(dir, "codex"), "ok")
	code, out := waitOn(t, dir)
	if code != job.ExitPartial || !strings.Contains(out, "member claude: never dispatched") {
		t.Fatalf("exit = %d, want %d, with claude never dispatched:\n%s", code, job.ExitPartial, out)
	}
}

// A turn whose final record is written but whose runner still holds its
// claim — it releases the session lock after the record — is still waited
// on, so a follow-up the wait's caller makes cannot meet a held session.
func TestWaitHoldsUntilTheClaimIsReleased(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	writeClaimedTurn(t, dir, "ok")
	claim, err := proc.LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { code, _ := waitOn(t, dir); done <- code }()
	select {
	case code := <-done:
		t.Fatalf("returned %d while the runner still held its claim", code)
	case <-time.After(150 * time.Millisecond):
	}
	claim.Release()
	select {
	case code := <-done:
		if code != job.ExitOK {
			t.Fatalf("exit = %d, want %d", code, job.ExitOK)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not return once the claim was released")
	}
}

// A turn written before runners claimed their directory is known only by its
// PID: the wait polls while the record says running and that PID answers, and
// returns once the record is final.
func TestWaitPollsARunnerKnownOnlyByItsPID(t *testing.T) {
	saved := waitPoll
	waitPoll = 20 * time.Millisecond
	defer func() { waitPoll = saved }()
	dir := filepath.Join(t.TempDir(), "job")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(status string) {
		meta := fmt.Sprintf(`{"schemaVersion":10,"status":%q,"provider":"codex","promptState":"accepted","timeoutMin":5,`+
			`"collectedAt":null,"runnerPid":%d}`, status, os.Getpid())
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("running")
	done := make(chan int, 1)
	go func() { code, _ := waitOn(t, dir); done <- code }()
	select {
	case code := <-done:
		t.Fatalf("returned %d while the record said running and its PID answered", code)
	case <-time.After(150 * time.Millisecond):
	}
	write("failed")
	select {
	case code := <-done:
		if code != job.ExitFailed {
			t.Fatalf("exit = %d, want %d", code, job.ExitFailed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not return once the record was final")
	}
}

// A fan-out that recorded its claim holds its name through a member that has
// not yet written a record only while that claim is held — the member may
// still start until then. Its recorded PID answering proves nothing.
func TestAClaimedFanOutHoldsForAnUnstartedMemberOnlyWhileClaimed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	writeClaimedGroup(t, dir, "codex")
	if hold, held := NameHold(dir); held {
		t.Fatalf("free claim, live PID: name held as %+v", hold)
	}
	claim, err := proc.LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Release()
	if hold, held := NameHold(dir); !held || hold.Kind != prose.HoldUnrecorded || hold.Member != "codex" {
		t.Fatalf("held claim: hold = %+v, held = %v; want the unstarted member to hold", hold, held)
	}
}

// A fan-out whose process is gone while a member's provider lives on is not
// something a wait can end: the wait returns at once and sends the caller to
// collect. So collect's closing line defers to each member's own action
// instead of to a wait, and the two never send a caller round in a loop.
func TestCollectDefersToMembersWhenTheFanOutsProcessIsGone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	writeClaimedGroup(t, dir, "codex")
	orphan := exec.Command("sleep", "30")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { orphan.Process.Kill(); orphan.Wait() }()
	meta := fmt.Sprintf(`{"schemaVersion":10,"status":"running","provider":"codex","promptState":"accepted","timeoutMin":5,`+
		`"collectedAt":null,"runnerPid":%d,"runnerLock":true,"providerPid":%d,"providerPgid":%d}`,
		os.Getpid(), orphan.Process.Pid, orphan.Process.Pid)
	if err := os.WriteFile(filepath.Join(dir, "codex", "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []Mode{ModeFull, ModeStatusOnly} {
		var out, errOut strings.Builder
		Collect(dir, "", mode, &out, &errOut)
		block := out.String()
		closing := block[strings.LastIndex(block, "\nnext: "):]
		if strings.Contains(closing, "envoy wait") || !strings.Contains(closing, "its own action in its section") {
			t.Errorf("mode %d: the closing line must defer to the members, not a wait:%s", mode, closing)
		}
		if !strings.Contains(block, "still alive") {
			t.Errorf("mode %d: the member's own section must say its provider is still alive:\n%s", mode, block)
		}
	}
}

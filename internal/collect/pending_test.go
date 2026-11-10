package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// A fan-out is listed as running — with a stop aimed at its process — only
// while that process holds its claim. Its recorded PID answering proves
// nothing: after a crash it can belong to any process, and the stop would hit
// a stranger while the members' undelivered results went unlisted.
func TestPendingReadsAFanOutsLivenessFromItsClaim(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	writeClaimedGroup(t, dir, "codex")
	writeClaimedTurn(t, filepath.Join(dir, "codex"), "ok")
	if item, listed := pendingGroup(dir, ""); !listed || item.label != "group" || item.stop != "" {
		t.Fatalf("claim free: entry = %+v (listed %v), want its uncollected member listed and no stop", item, listed)
	}
	claim, err := proc.LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Release()
	if item, listed := pendingGroup(dir, ""); !listed || item.label != "running" || item.stop == "" {
		t.Fatalf("claim held: entry = %+v (listed %v), want it listed as running with its stop", item, listed)
	}
}

// Another version's fan-out is listed by what its members' own stamps say it
// owes, once its own engine is not provably still running it — never as
// damage, and never on the strength of a directory that shows nothing owed.
func TestPendingReadsAnotherSchemasFanOut(t *testing.T) {
	gone := deadPid(t)
	cases := []struct {
		name   string
		build  func(dir string)
		listed bool
	}{
		{"its runner alive, a member uncollected", func(d string) {
			writeOtherGroup(t, d, os.Getpid())
			writeTurn(t, filepath.Join(d, "codex"), "ok", false)
		}, false},
		{"its runner gone, a member uncollected", func(d string) {
			writeOtherGroup(t, d, gone)
			writeTurn(t, filepath.Join(d, "codex"), "ok", false)
		}, true},
		{"every member collected", func(d string) {
			writeOtherGroup(t, d, gone)
			writeTurn(t, filepath.Join(d, "codex"), "ok", true)
		}, false},
		{"a member whose record will not read", func(d string) {
			writeOtherGroup(t, d, gone)
			os.MkdirAll(filepath.Join(d, "codex"), 0o755)
			os.WriteFile(filepath.Join(d, "codex", "meta.json"), []byte("{"), 0o644)
		}, true},
		{"only members that never wrote a record", func(d string) {
			writeOtherGroup(t, d, gone)
			os.MkdirAll(filepath.Join(d, "codex"), 0o755)
		}, false},
		{"a directory that cannot be listed", func(d string) {
			writeOtherGroup(t, d, 0)
			os.Chmod(d, 0o300)
			t.Cleanup(func() { os.Chmod(d, 0o755) })
		}, true},
	}
	for _, c := range cases {
		dir := filepath.Join(t.TempDir(), "job")
		c.build(dir)
		item, listed := pendingGroup(dir, "")
		if listed != c.listed {
			t.Errorf("%s: listed = %v, want %v (%+v)", c.name, listed, c.listed, item)
			continue
		}
		if listed && (item.label != "other-schema" || item.next != prose.PendingOtherSchemaFanNext(dir)) {
			t.Errorf("%s: entry = %+v, want another version's fan-out sent to its members' files", c.name, item)
		}
	}
}

// A fan-out whose manifest recorded no claim is known only by its PID, and a
// PID is evidence only while the work it supervised is unfinished: once every
// member has ended, the number may since belong to any process. collect,
// pending and wait read that one way — no stop aimed at a stranger, and no
// "running" entry hiding results nobody collected — while a member still
// recorded as running leaves the PID the only evidence there is.
func TestAFanOutKnownOnlyByItsPIDIsLiveOnlyWhileItsWorkIsUnfinished(t *testing.T) {
	stop := "stop: " + prose.StopCommand(os.Getpid())
	read := func(dir string) (block string, item pendingItem, listed bool) {
		var out, errOut strings.Builder
		collectDir(dir, "", ModeStatusOnly, &out, &errOut)
		item, listed = pendingGroup(dir, "")
		return out.String(), item, listed
	}

	// This test process stands in for a PID the system handed to another
	// process after the fan-out ended.
	finished := filepath.Join(t.TempDir(), "job")
	writeGroup(t, finished, os.Getpid(), "codex", "claude")
	writeTurn(t, filepath.Join(finished, "codex"), "ok", false)
	writeTurn(t, filepath.Join(finished, "claude"), "failed", false)
	block, item, listed := read(finished)
	if strings.Contains(block, stop) {
		t.Errorf("every member ended: collect must not aim a stop at the recorded PID:\n%s", block)
	}
	if !listed || item.label != "group" || item.stop != "" {
		t.Errorf("every member ended: pending entry = %+v (listed %v), want its uncollected members listed and no stop", item, listed)
	}
	if code, out := waitEnds(t, finished); code != job.ExitPartial {
		t.Errorf("every member ended: wait exit = %d, want %d\n%s", code, job.ExitPartial, out)
	}

	unfinished := filepath.Join(t.TempDir(), "job")
	writeGroup(t, unfinished, os.Getpid(), "codex", "claude")
	writeTurn(t, filepath.Join(unfinished, "codex"), "ok", false)
	writeTurn(t, filepath.Join(unfinished, "claude"), "running", false)
	block, item, listed = read(unfinished)
	if !strings.Contains(block, stop) {
		t.Errorf("a member still running: collect must offer the stop:\n%s", block)
	}
	if !listed || item.label != "running" || item.stop == "" {
		t.Errorf("a member still running: pending entry = %+v (listed %v), want it listed as running with its stop", item, listed)
	}
}

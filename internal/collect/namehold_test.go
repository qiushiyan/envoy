package collect

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/qiushiyan/envoy/internal/prose"
)

func writeTurn(t *testing.T, dir, status string, collected bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := "null"
	if collected {
		stamp = `"2026-09-21T00:00:00.000Z"`
	}
	meta := fmt.Sprintf(`{"schemaVersion":10,"status":%q,"provider":"codex","promptState":"accepted","timeoutMin":5,"collectedAt":%s}`, status, stamp)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

// deadPid is a process id that was live a moment ago and is provably gone.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func writeGroup(t *testing.T, dir string, runnerPid int, members ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	list := ""
	for i, m := range members {
		if i > 0 {
			list += ","
		}
		list += fmt.Sprintf("%q", m)
	}
	group := fmt.Sprintf(`{"schemaVersion":2,"startedAt":"2026-09-21T00:00:00.000Z","cwd":"/tmp","timeoutMin":5,"gitBaseline":null,"members":[%s],"runnerPid":%d}`, list, runnerPid)
	if err := os.WriteFile(filepath.Join(dir, "group.json"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeOtherGroup writes a manifest of a group schema this engine does not
// read, whose roster entries are shaped as no schema of this engine's are.
func writeOtherGroup(t *testing.T, dir string, runnerPid int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	group := fmt.Sprintf(`{"schemaVersion":3,"members":[{"name":"codex"}],"runnerPid":%d}`, runnerPid)
	if err := os.WriteFile(filepath.Join(dir, "group.json"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Another version's fan-out holds or releases its name on its stamp alone:
// what its directory lists is no part of that decision, so a directory that
// cannot be listed releases a name its stamp releases.
func TestAnotherSchemasFanOutHoldsOnItsStampAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	writeOtherGroup(t, dir, 0)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if hold, held := NameHold(dir); held {
		t.Fatalf("NameHold = %+v; a stamp recording no runner holds nothing, listable or not", hold)
	}
}

// A name is released by delivery and by nothing else: not by success, not by
// the first member being done, and never by a record that is missing or
// unreadable where one could still appear.
func TestNameHoldClassifiesWhatStillHoldsAName(t *testing.T) {
	gone := deadPid(t)
	cases := []struct {
		name  string
		build func(dir string)
		want  prose.NameHold
		held  bool
	}{
		{"collected ok turn", func(d string) { writeTurn(t, d, "ok", true) }, prose.NameHold{}, false},
		{"collected failed turn", func(d string) { writeTurn(t, d, "failed", true) }, prose.NameHold{}, false},
		{"uncollected turn", func(d string) { writeTurn(t, d, "ok", false) }, prose.NameHold{Kind: prose.HoldUncollected}, true},
		{"running turn, even once collected", func(d string) { writeTurn(t, d, "running", true) }, prose.NameHold{Kind: prose.HoldRunning}, true},
		{"empty reservation", func(d string) { os.MkdirAll(d, 0o755) }, prose.NameHold{Kind: prose.HoldUnrecorded, Empty: true}, true},
		{"recordless directory with files", func(d string) {
			os.MkdirAll(d, 0o755)
			os.WriteFile(filepath.Join(d, "prompt.md"), []byte("x"), 0o644)
		}, prose.NameHold{Kind: prose.HoldUnrecorded}, true},
		{"unreadable record", func(d string) {
			os.MkdirAll(d, 0o755)
			os.WriteFile(filepath.Join(d, "meta.json"), []byte("{not json"), 0o644)
		}, prose.NameHold{Kind: prose.HoldUnreadable}, true},
		{"fan-out, every member delivered", func(d string) {
			writeGroup(t, d, os.Getpid(), "codex", "claude")
			writeTurn(t, filepath.Join(d, "codex"), "ok", true)
			writeTurn(t, filepath.Join(d, "claude"), "failed", true)
		}, prose.NameHold{}, false},
		{"fan-out held by its last member", func(d string) {
			writeGroup(t, d, gone, "codex", "claude")
			writeTurn(t, filepath.Join(d, "codex"), "ok", true)
			writeTurn(t, filepath.Join(d, "claude"), "ok", false)
		}, prose.NameHold{Kind: prose.HoldUncollected, Member: "claude"}, true},
		{"fan-out with a running member", func(d string) {
			writeGroup(t, d, gone, "codex", "claude")
			writeTurn(t, filepath.Join(d, "codex"), "ok", true)
			writeTurn(t, filepath.Join(d, "claude"), "running", false)
		}, prose.NameHold{Kind: prose.HoldRunning, Member: "claude"}, true},
		// A member with no record: absence does not say it was refused. While
		// the supervising process may be alive the member may yet start.
		{"fan-out alive, one member has no record yet", func(d string) {
			writeGroup(t, d, os.Getpid(), "codex", "claude")
			writeTurn(t, filepath.Join(d, "claude"), "ok", true)
		}, prose.NameHold{Kind: prose.HoldUnrecorded, Member: "codex"}, true},
		{"fan-out gone, its refused member never wrote a record", func(d string) {
			writeGroup(t, d, gone, "codex", "claude")
			writeTurn(t, filepath.Join(d, "claude"), "ok", true)
		}, prose.NameHold{}, false},
		{"fan-out gone, every member refused", func(d string) {
			writeGroup(t, d, gone, "codex", "claude")
		}, prose.NameHold{}, false},
		{"fan-out in its first moments", func(d string) {
			writeGroup(t, d, os.Getpid(), "codex", "claude")
		}, prose.NameHold{Kind: prose.HoldUnrecorded, Member: "codex"}, true},
		{"older manifest that names no supervisor", func(d string) {
			writeGroup(t, d, 0, "codex", "claude")
			writeTurn(t, filepath.Join(d, "claude"), "ok", true)
		}, prose.NameHold{Kind: prose.HoldUnrecorded, Member: "codex"}, true},
		// Another version's fan-out holds like another version's turn: while
		// the runner its stamp records may be alive, and never past it, since
		// this engine can deliver nothing under the name.
		{"another schema's fan-out, its runner alive", func(d string) {
			writeOtherGroup(t, d, os.Getpid())
		}, prose.NameHold{Kind: prose.HoldOtherVersion}, true},
		{"another schema's turn, its runner alive", func(d string) {
			os.MkdirAll(d, 0o755)
			meta := fmt.Sprintf(`{"schemaVersion":9,"status":"running","provider":"codex","runnerPid":%d}`, os.Getpid())
			os.WriteFile(filepath.Join(d, "meta.json"), []byte(meta), 0o644)
		}, prose.NameHold{Kind: prose.HoldOtherVersion}, true},
		{"another schema's fan-out, its runner gone", func(d string) {
			writeOtherGroup(t, d, gone)
			writeTurn(t, filepath.Join(d, "codex"), "ok", false)
		}, prose.NameHold{}, false},
		{"another schema's fan-out recording no runner", func(d string) {
			writeOtherGroup(t, d, 0)
		}, prose.NameHold{}, false},
		{"fan-out with an unreadable roster", func(d string) {
			os.MkdirAll(d, 0o755)
			os.WriteFile(filepath.Join(d, "group.json"), []byte("{"), 0o644)
		}, prose.NameHold{Kind: prose.HoldUnreadable}, true},
	}
	for _, c := range cases {
		dir := filepath.Join(t.TempDir(), "job")
		c.build(dir)
		got, held := NameHold(dir)
		if held != c.held || got != c.want {
			t.Errorf("%s: NameHold = %+v held=%v, want %+v held=%v", c.name, got, held, c.want, c.held)
		}
	}
}

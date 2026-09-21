package collect

import (
	"fmt"
	"os"
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
	meta := fmt.Sprintf(`{"schemaVersion":9,"status":%q,"provider":"codex","promptState":"accepted","timeoutMin":5,"collectedAt":%s}`, status, stamp)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGroup(t *testing.T, dir string, members ...string) {
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
	group := fmt.Sprintf(`{"schemaVersion":2,"startedAt":"2026-09-21T00:00:00.000Z","cwd":"/tmp","timeoutMin":5,"gitBaseline":null,"members":[%s]}`, list)
	if err := os.WriteFile(filepath.Join(dir, "group.json"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A name is released by delivery and by nothing else: not by success, not by
// the first member being done, and never by a record that is missing or
// unreadable where one could still appear.
func TestNameHoldClassifiesWhatStillHoldsAName(t *testing.T) {
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
			writeGroup(t, d, "codex", "claude")
			writeTurn(t, filepath.Join(d, "codex"), "ok", true)
			writeTurn(t, filepath.Join(d, "claude"), "failed", true)
		}, prose.NameHold{}, false},
		{"fan-out held by its last member", func(d string) {
			writeGroup(t, d, "codex", "claude")
			writeTurn(t, filepath.Join(d, "codex"), "ok", true)
			writeTurn(t, filepath.Join(d, "claude"), "ok", false)
		}, prose.NameHold{Kind: prose.HoldUncollected, Member: "claude"}, true},
		{"fan-out with a running member", func(d string) {
			writeGroup(t, d, "codex", "claude")
			writeTurn(t, filepath.Join(d, "codex"), "ok", true)
			writeTurn(t, filepath.Join(d, "claude"), "running", false)
		}, prose.NameHold{Kind: prose.HoldRunning, Member: "claude"}, true},
		{"fan-out whose refused member never wrote a record", func(d string) {
			writeGroup(t, d, "codex", "claude")
			writeTurn(t, filepath.Join(d, "claude"), "ok", true)
		}, prose.NameHold{}, false},
		{"fan-out in its first moments: no member has a record", func(d string) {
			writeGroup(t, d, "codex", "claude")
		}, prose.NameHold{Kind: prose.HoldUnrecorded}, true},
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

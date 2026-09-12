package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The aggregate code is what an agent branches on before it reads anything, so
// each case has to license the right move: 0 = every result is here, 6 = some
// results are here and the rest need per-member decisions, and a single code
// only when no member produced anything.
func TestExitCodeForGroup(t *testing.T) {
	cases := []struct {
		name  string
		codes []int
		want  int
	}{
		{"all ok", []int{ExitOK, ExitOK}, ExitOK},
		{"one ok, one timed out", []int{ExitOK, ExitTimeout}, ExitPartial},
		{"one ok among many failures", []int{ExitFailed, ExitOK, ExitInfra}, ExitPartial},
		{"both timed out", []int{ExitTimeout, ExitTimeout}, ExitTimeout},
		{"both failed", []int{ExitFailed, ExitFailed}, ExitFailed},
		// No member returned a result: the dispatch-side cause is reported,
		// because that is the one fixed by changing the command.
		{"locked session outranks a provider failure", []int{ExitUsage, ExitFailed}, ExitUsage},
		{"infra outranks the cap", []int{ExitTimeout, ExitInfra}, ExitInfra},
		{"an interrupt outranks the cap", []int{ExitTimeout, ExitInterrupted}, ExitInterrupted},
		{"no members at all", nil, ExitInfra},
	}
	for _, c := range cases {
		if got := ExitCodeForGroup(c.codes); got != c.want {
			t.Errorf("%s: ExitCodeForGroup(%v) = %d, want %d", c.name, c.codes, got, c.want)
		}
	}
}

// group.json is a roster, and a member's settings and outcome are only ever
// read from that member's own meta.json. Any member field here would be a
// second copy of a fact the turn owns, free to drift from it — and another
// schema means another engine's records, refused rather than reinterpreted.
func TestGroupManifestIsARosterOnly(t *testing.T) {
	dir := t.TempDir()
	gw := GroupWorkspace{Dir: dir}
	g := &Group{
		SchemaVersion: GroupSchemaVersion,
		StartedAt:     "2026-07-26T00:00:00.000Z",
		Members:       []string{"codex", "claude-opus"},
	}
	if err := g.WriteFile(gw.GroupPath()); err != nil {
		t.Fatal(err)
	}
	if !IsGroupDir(dir) {
		t.Fatal("a dir holding group.json must read as a fan-out")
	}
	if IsGroupDir(t.TempDir()) {
		t.Fatal("a dir without group.json must not read as a fan-out")
	}
	got, err := ReadGroupFile(gw.GroupPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 2 || got.Members[1] != "claude-opus" {
		t.Fatalf("members round trip = %+v", got.Members)
	}
	// The member's job dir is an ordinary one named after the member, so
	// every single-turn path holds.
	if want := filepath.Join(dir, "codex", "result.md"); gw.Member("codex").ResultPath() != want {
		t.Fatalf("member result path = %s, want %s", gw.Member("codex").ResultPath(), want)
	}
	if err := os.WriteFile(gw.GroupPath(), []byte(`{"schemaVersion":1,"members":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGroupFile(gw.GroupPath()); err == nil || !strings.Contains(err.Error(), "schema 1") {
		t.Fatalf("another schema must be refused by name, got %v", err)
	}
}

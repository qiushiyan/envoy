package job

import (
	"errors"
	"os"
	"slices"
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
		{"a refused dispatch outranks infra", []int{ExitInfra, ExitUsage}, ExitUsage},
		{"infra outranks an interrupt", []int{ExitInterrupted, ExitInfra}, ExitInfra},
		{"the cap outranks a provider failure", []int{ExitFailed, ExitTimeout}, ExitTimeout},
		{"no members at all", nil, ExitInfra},
	}
	for _, c := range cases {
		if got := ExitCodeForGroup(c.codes); got != c.want {
			t.Errorf("%s: ExitCodeForGroup(%v) = %d, want %d", c.name, c.codes, got, c.want)
		}
	}
}

// Another version's manifest is refused, but its stamp is not: who
// dispatched it and the runner supervising it mean what they always have, so
// naming reads them from any version — and from a manifest of this version
// whose roster is damaged, as a turn's stamp is read past its damage. The
// roster changed shape between schemas and is never read; another version's
// members are the subdirectories that hold a record, as a store's jobs are.
func TestAnotherSchemasFanOutKeepsItsStamp(t *testing.T) {
	dir := t.TempDir()
	gw := GroupWorkspace{Dir: dir}
	manifest := `{"schemaVersion":1,"caller":"session-a","runnerPid":42,"members":[{"name":"codex","provider":"codex"}]}`
	if err := os.WriteFile(gw.GroupPath(), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(gw.Member("codex").Dir, 0o755)
	old := `{"schemaVersion":9,"status":"ok","provider":"codex","collectedAt":null}`
	if err := os.WriteFile(gw.Member("codex").MetaPath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(gw.Member("claude").Dir, 0o755) // no record: not a member anyone can see

	if caller, err := CallerOf(dir); err != nil || caller != "session-a" {
		t.Fatalf("CallerOf = %q, %v; want the stamp's caller", caller, err)
	}
	stamp, group, err := ReadGroupRecord(dir)
	var other *SchemaError
	if !errors.As(err, &other) || other.Record != "group.json" || other.Version != 1 || !slices.Equal(other.Reads, []int{GroupSchemaVersion}) {
		t.Fatalf("ReadGroupRecord err = %v, want group.json's *SchemaError", err)
	}
	if stamp == nil || stamp.RunnerPid != 42 || group != nil {
		t.Fatalf("ReadGroupRecord = %+v, %+v; want the stamp and no roster", stamp, group)
	}
	if fan, err := ReadFan(dir); fan != nil || !errors.As(err, &other) {
		t.Fatalf("ReadFan = %+v, %v; another version's fan-out is refused whole", fan, err)
	}
	members, err := RecordedMembers(dir)
	if err != nil || len(members) != 1 || members[0].Name != "codex" || members[0].Stamp.CollectedAt != nil {
		t.Fatalf("RecordedMembers = %+v, %v; want codex found by its record", members, err)
	}

	damaged := `{"schemaVersion":2,"caller":"session-b","members":5}`
	if err := os.WriteFile(gw.GroupPath(), []byte(damaged), 0o644); err != nil {
		t.Fatal(err)
	}
	if caller, err := CallerOf(dir); err != nil || caller != "session-b" {
		t.Fatalf("CallerOf over a damaged roster = %q, %v; want the stamp's caller", caller, err)
	}
	if _, err := ReadFan(dir); err == nil {
		t.Fatal("a damaged roster must not read as a fan-out")
	}
}

// A reader must tell absence from damage: a directory with no record is not a
// job yet, while a record that is there and will not read is evidence nobody
// may read past. ReadFan carries each member with its own read result, so the
// roster's word that a member was meant to run survives a missing record.
func TestReadersSeparateAbsenceFromDamage(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadMeta(dir); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("no meta.json must read as ErrNoRecord, got %v", err)
	}
	if _, err := ReadFan(dir); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("no group.json must read as ErrNoRecord, got %v", err)
	}
	if HasRecord(dir) {
		t.Fatal("an empty directory holds no record")
	}

	gw := GroupWorkspace{Dir: dir}
	group := &Group{SchemaVersion: GroupSchemaVersion, Members: []string{"codex", "claude", "broken"}}
	if err := group.WriteFile(gw.GroupPath()); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(gw.Member("codex").Dir, 0o755)
	ok := &Meta{SchemaVersion: MetaSchemaVersion, Status: StatusOK, Provider: "codex"}
	if err := ok.WriteFile(gw.Member("codex").MetaPath()); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(gw.Member("broken").Dir, 0o755)
	if err := os.WriteFile(gw.Member("broken").MetaPath(), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasRecord(dir) {
		t.Fatal("a fan-out with group.json holds a record")
	}
	fan, err := ReadFan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fan.Members) != 3 {
		t.Fatalf("every roster member is carried, got %+v", fan.Members)
	}
	if m := fan.Members[0]; m.Err != nil || m.Meta.Status != StatusOK || m.Dir != gw.Member("codex").Dir {
		t.Fatalf("readable member = %+v", m)
	}
	if m := fan.Members[1]; !errors.Is(m.Err, ErrNoRecord) || m.Meta != nil {
		t.Fatalf("recordless member = %+v", m)
	}
	if m := fan.Members[2]; m.Err == nil || errors.Is(m.Err, ErrNoRecord) {
		t.Fatalf("an unreadable record is damage, not absence: %+v", m)
	}
}

// Only a fan-out read whole establishes that its work is finished: every
// member the roster names has a record whose stamp says its turn is over, in
// whatever schema it was written. A member with no record may still start and
// one that does not parse says nothing, so either leaves the work unfinished —
// and a manifest's stamp alone, with no roster to read, never establishes it.
func TestAFanOutsWorkIsFinishedOnlyWhenEveryMemberSaysSo(t *testing.T) {
	member := func(dir, name, meta string) Member {
		md := GroupWorkspace{Dir: dir}.Member(name).Dir
		os.MkdirAll(md, 0o755)
		if meta != "" {
			if err := os.WriteFile(Workspace{Dir: md}.MetaPath(), []byte(meta), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return Member{Name: name, Record: ReadRecord(md)}
	}
	const (
		ended   = `{"schemaVersion":10,"status":"ok","provider":"codex"}`
		running = `{"schemaVersion":10,"status":"running","provider":"codex"}`
		older   = `{"schemaVersion":9,"status":"failed","provider":"codex"}`
	)
	cases := []struct {
		name     string
		members  func(dir string) []Member
		finished bool
	}{
		{"every member ended", func(d string) []Member {
			return []Member{member(d, "codex", ended), member(d, "claude", ended)}
		}, true},
		{"one ended in another schema", func(d string) []Member {
			return []Member{member(d, "codex", ended), member(d, "claude", older)}
		}, true},
		{"one still running", func(d string) []Member {
			return []Member{member(d, "codex", ended), member(d, "claude", running)}
		}, false},
		{"one never wrote a record", func(d string) []Member {
			return []Member{member(d, "codex", ended), member(d, "claude", "")}
		}, false},
		{"one whose record does not parse", func(d string) []Member {
			return []Member{member(d, "codex", ended), member(d, "claude", "{")}
		}, false},
	}
	for _, c := range cases {
		dir := t.TempDir()
		fan := Fan{Group: &Group{RunnerPid: 42}, Members: c.members(dir)}
		if got := fan.Runner(dir); got.Finished != c.finished || got.Pid != 42 || got.Dir != dir {
			t.Errorf("%s: Runner = %+v, want finished %v", c.name, got, c.finished)
		}
	}
	if got := (&GroupStamp{RunnerPid: 42}).Runner("d"); got.Finished {
		t.Errorf("a manifest's stamp alone must not establish its work finished: %+v", got)
	}
}

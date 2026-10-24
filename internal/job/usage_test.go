package job

import (
	"os"
	"strings"
	"testing"
)

// A turn's usage starts as an empty issue list, and meta.json carries it that
// way: a program reading the record iterates [], where null would read as a
// field the engine never filled.
func TestAFreshUsagePersistsAnEmptyIssueList(t *testing.T) {
	dir := t.TempDir()
	m := &Meta{SchemaVersion: MetaSchemaVersion, Status: StatusOK, Provider: "claude", Usage: NewUsage()}
	if err := m.WriteFile(Workspace{Dir: dir}.MetaPath()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(Workspace{Dir: dir}.MetaPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"issues": []`) {
		t.Fatalf("a fresh usage must persist an empty issue list:\n%s", data)
	}
	got, err := ReadMeta(dir)
	if err != nil || got.Usage.State != UsageUnmeasured || got.Usage.Issues == nil {
		t.Fatalf("usage did not round-trip: %+v, %v", got.Usage, err)
	}
}

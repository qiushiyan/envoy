package job

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestUsageSchemaCompatibility(t *testing.T) {
	// Usage is optional within a schema: schema 10 replaced the worded failure
	// fields, and an additive observation like usage bumps nothing.
	if MetaSchemaVersion != 10 {
		t.Fatal("additive usage must not move the schema; only a replaced field does")
	}
	p := filepath.Join(t.TempDir(), "meta.json")
	if err := os.WriteFile(p, []byte(`{"schemaVersion":10,"status":"ok","provider":"claude"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMetaFile(p)
	if err != nil || m.Usage != nil {
		t.Fatalf("a record without usage must read: %v", err)
	}
	m.Usage = NewUsage()
	if err := m.WriteFile(p); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	// An existing reader consuming only known fields still reads the new record.
	var older struct {
		SchemaVersion int    `json:"schemaVersion"`
		Status        string `json:"status"`
	}
	if err := json.Unmarshal(data, &older); err != nil || older.SchemaVersion != MetaSchemaVersion || older.Status != "ok" {
		t.Fatalf("existing fields changed: %v", err)
	}
	m, err = ReadMetaFile(p)
	if err != nil || m.Usage.State != "unmeasured" || m.Usage.Issues == nil {
		t.Fatalf("usage did not round-trip: %v", err)
	}
	m.Usage = nil
	data, _ = m.Marshal()
	var fields map[string]any
	json.Unmarshal(data, &fields)
	if _, ok := fields["usage"]; ok {
		t.Fatal("unsupported usage must be absent")
	}
}

func TestUsageEndPreservesEvidence(t *testing.T) {
	u := NewUsage()
	u.LatestContextTokens, u.OutputTokens = Ptr(int64(100)), nil
	u.End()
	u.End()
	if !u.Final || u.State != "incomplete" || *u.LatestContextTokens != 100 || u.OutputTokens != nil || !slices.Equal(u.Issues, []string{"missing_terminal"}) {
		t.Fatalf("ended stream = %+v", u)
	}
	u = &Usage{Final: true, State: "settled", OutputTokens: Ptr(int64(42))}
	u.End()
	if u.State != "settled" || *u.OutputTokens != 42 {
		t.Fatal("cleanup must preserve terminal accounting")
	}
}

package job

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCommandPrefixSchemaCompatibility(t *testing.T) {
	if MetaSchemaVersion != 9 {
		t.Fatal("additive command observation must preserve schema 9")
	}
	p := filepath.Join(t.TempDir(), "meta.json")
	old := `{"schemaVersion":9,"status":"ok","provider":"codex","providerArgv":["codex","exec","--json","-"]}`
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMetaFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.CommandPrefix != nil {
		t.Fatal("older command prefix must remain unknown")
	}
	prefix := []string{"launcher", "codex", "--"}
	m.CommandPrefix = prefix
	if err := m.WriteFile(p); err != nil {
		t.Fatal(err)
	}
	m, err = ReadMetaFile(p)
	if err != nil || !slices.Equal(m.CommandPrefix, prefix) {
		t.Fatalf("prefix did not round trip: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var older struct {
		SchemaVersion int      `json:"schemaVersion"`
		ProviderArgv  []string `json:"providerArgv"`
	}
	if err := json.Unmarshal(data, &older); err != nil || older.SchemaVersion != 9 || !slices.Equal(older.ProviderArgv, []string{"codex", "exec", "--json", "-"}) {
		t.Fatalf("existing argv semantics changed: %+v (%v)", older, err)
	}
	m.SchemaVersion = 8
	if err := m.WriteFile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMetaFile(p); err == nil {
		t.Fatal("other schema version was accepted")
	}
}

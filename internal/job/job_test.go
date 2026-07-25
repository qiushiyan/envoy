package job

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestISOFormat(t *testing.T) {
	got := ISO(time.Date(2026, 7, 25, 10, 30, 0, 123_000_000, time.UTC))
	if got != "2026-07-25T10:30:00.123Z" {
		t.Fatalf("ISO = %q", got)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meta.json")
	if err := WriteFileAtomic(p, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("two")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "two" {
		t.Fatalf("content = %q", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files must not survive, dir has %d entries", len(entries))
	}
}

func TestResolveOutDirInRepo(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	now := time.Date(2026, 7, 25, 11, 0, 0, 0, time.Local)

	dir, err := ResolveOutDir("", repo, "My Label!", "codex", now)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(realPath(t, repo), ".envoy")
	if filepath.Dir(realPath(t, dir)) != base {
		t.Fatalf("dir = %q, want under %q", dir, base)
	}
	if !strings.HasSuffix(dir, "20260725-110000-my-label-") && !strings.HasSuffix(dir, "20260725-110000-my-label") {
		t.Fatalf("dir name = %q", filepath.Base(dir))
	}
	ignore, err := os.ReadFile(filepath.Join(base, ".gitignore"))
	if err != nil || string(ignore) != "*\n" {
		t.Fatalf("self-gitignore missing: %v %q", err, ignore)
	}

	// A same-second collision must uniquify, not reuse.
	dir2, err := ResolveOutDir("", repo, "My Label!", "codex", now)
	if err != nil {
		t.Fatal(err)
	}
	if dir2 == dir {
		t.Fatal("colliding stamps must not share a dir")
	}
}

func TestResolveOutDirOutsideRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	dir, err := ResolveOutDir("", cwd, "", "claude", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wantBase := filepath.Join(home, ".local", "state", "envoy", filepath.Base(cwd))
	if filepath.Dir(dir) != wantBase {
		t.Fatalf("dir = %q, want under %q", dir, wantBase)
	}
	if !strings.Contains(filepath.Base(dir), "-claude") {
		t.Fatalf("label must default to provider: %q", dir)
	}
}

func TestWorkspacePrepareAndProgress(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "p.md")
	os.WriteFile(prompt, []byte("do the thing"), 0o644)
	ws := Workspace{Dir: dir}
	if err := ws.Prepare(prompt); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(ws.PromptPath())
	if string(data) != "do the thing" {
		t.Fatalf("prompt copy = %q", data)
	}

	pl := ws.Progress()
	pl.Append("starting", KV{K: "provider", V: "codex"}, KV{K: "skipme", V: nil}, KV{K: "note", V: "two words"})
	line, _ := os.ReadFile(ws.ProgressLogPath())
	re := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z state=starting provider=codex note=two_words\n$`)
	if !re.Match(line) {
		t.Fatalf("progress line = %q", line)
	}
}

func TestMetaRoundTripAndNullDetection(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meta.json")
	m := &Meta{SchemaVersion: MetaSchemaVersion, Status: StatusOK, Provider: "codex", PromptState: PromptAccepted, ResultKind: ResultFinal}
	if err := m.WriteFile(p); err != nil {
		t.Fatal(err)
	}
	got, raw, err := ReadMetaFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOK || got.Provider != "codex" {
		t.Fatalf("round trip = %+v", got)
	}
	if v, ok := raw["collectedAt"]; !ok || string(v) != "null" {
		t.Fatalf("collectedAt must serialize as explicit null, got %q (present=%v)", v, ok)
	}
	if got.ReconciledAt != nil {
		t.Fatal("reconciledAt must stay absent until reconciliation")
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "reconciledAt") {
		t.Fatal("reconciledAt must be omitted when unset")
	}
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

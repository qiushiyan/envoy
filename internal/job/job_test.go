package job

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

// A bare name is an address in the invoking project's central store; a path
// is taken as given; and nothing is ever written inside the project tree.
func TestResolveRefNamesLandInTheCentralStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q")
	subdir := filepath.Join(repo, "pkg", "inner")
	os.MkdirAll(subdir, 0o755)

	dir, err := ResolveName("review-r1", repo)
	if err != nil {
		t.Fatal(err)
	}
	// A reference reads the same name, and reaches a fan-out's member.
	if ref, _ := ResolveRef("review-r1", repo); ref != dir {
		t.Fatalf("ResolveRef = %q, want %q", ref, dir)
	}
	if ref, _ := ResolveRef("review-r1/codex", repo); ref != filepath.Join(dir, "codex") {
		t.Fatalf("member ref = %q", ref)
	}
	if _, err := ResolveName("review-r1/codex", repo); err == nil {
		t.Fatal("a new job's name is one segment")
	}
	jobsRoot := filepath.Join(home, ".local", "state", "envoy", "jobs")
	if filepath.Dir(filepath.Dir(dir)) != jobsRoot || filepath.Base(dir) != "review-r1" {
		t.Fatalf("dir = %q, want %q/<project>/review-r1", dir, jobsRoot)
	}
	// The name round-trips exactly: no lowercasing, no slugging.
	if d, _ := ResolveName("Review.R1_x", repo); filepath.Base(d) != "Review.R1_x" {
		t.Fatalf("name was rewritten: %q", d)
	}
	// A subdirectory dispatch belongs to the same project store.
	if sub, _ := ResolveName("review-r1", subdir); sub != dir {
		t.Fatalf("subdir resolves %q, want %q", sub, dir)
	}
	// A path is a path.
	if p, _ := ResolveRef("./jobs/x", repo); !filepath.IsAbs(p) || filepath.Base(p) != "x" {
		t.Fatalf("path ref = %q", p)
	}
	for _, bad := range []string{"", "-lead", "a b", "a/b/../c!"} {
		if _, err := ResolveRef(bad, repo); err == nil {
			t.Fatalf("ResolveRef(%q) must refuse", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".envoy")); !os.IsNotExist(err) {
		t.Fatal("central storage must not create dirs inside the repo")
	}
}

// A name is used once. Reserving it twice — from two dispatches racing for
// the same name, or one re-run — must succeed exactly once, and creation
// itself is the check: a stat-then-create window loses this race.
func TestReserveIsAtomicAndRefusesAnExistingName(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "proj", "review-r1")

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = Reserve(dir) })
	}
	wg.Wait()
	won := 0
	for i := range n {
		switch {
		case errs[i] == nil:
			won++
		case !errors.Is(errs[i], ErrJobExists):
			t.Fatalf("reserve %d: %v", i, errs[i])
		}
	}
	if won != 1 {
		t.Fatalf("%d reservations won, want exactly 1", won)
	}
	if err := Reserve(dir); !errors.Is(err, ErrJobExists) {
		t.Fatalf("second reservation = %v, want ErrJobExists", err)
	}
	// A refusal before anything ran gives the name back; content keeps it.
	Unreserve(dir)
	if err := Reserve(dir); err != nil {
		t.Fatalf("after Unreserve: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{}"), 0o644)
	Unreserve(dir)
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("Unreserve must not remove a directory with content")
	}
}

func TestProjectSlugDistinguishesSameName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parentA, parentB := t.TempDir(), t.TempDir()
	a := filepath.Join(parentA, "myproj")
	b := filepath.Join(parentB, "myproj")
	os.MkdirAll(a, 0o755)
	os.MkdirAll(b, 0o755)

	slugA, slugB := ProjectSlug(a), ProjectSlug(b)
	if !strings.HasPrefix(slugA, "myproj-") || !strings.HasPrefix(slugB, "myproj-") {
		t.Fatalf("slugs must lead with the basename: %q %q", slugA, slugB)
	}
	if slugA == slugB {
		t.Fatal("same-named projects at different paths must not share a store")
	}
	if ProjectSlug(a) != slugA {
		t.Fatal("slug must be stable across calls")
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

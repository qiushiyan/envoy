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

// A directory is reserved once. Reserving it twice — from two dispatches racing
// for the same name, or one re-run — must succeed exactly once, and creation
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
}

// A name addresses its highest generation, and only the spelling
// GenerationDir writes counts as one: a caller-named neighbour, a file, or a
// look-alike suffix never captures the name.
func TestLatestResolvesTheHighestGeneration(t *testing.T) {
	base := t.TempDir()
	if dir, g := Latest(base, "review-r1"); g != 0 || dir != filepath.Join(base, "review-r1") {
		t.Fatalf("free name = %q gen %d, want the bare name at generation 0", dir, g)
	}
	for _, d := range []string{"review-r1", "review-r1+2", "review-r1+10", "review-r1b", "review-r1+02", "review-r1+1", "review-r1+x", "review-r10+40"} {
		if err := os.Mkdir(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "review-r1+99"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if dir, g := Latest(base, "review-r1"); g != 10 || dir != filepath.Join(base, "review-r1+10") {
		t.Fatalf("Latest = %q gen %d, want review-r1+10 gen 10", dir, g)
	}
	if got := GenerationDir(base, "review-r1", 11); got != filepath.Join(base, "review-r1+11") {
		t.Fatalf("GenerationDir = %q", got)
	}
	// A generation is reachable by path only: no caller can name one.
	if _, err := ResolveName("review-r1+2", base); err == nil {
		t.Fatal("a generation must not be a valid job name")
	}
}

// References follow the name to its latest generation, members included.
func TestResolveRefFollowsTheLatestGeneration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	first, _ := ResolveName("consult-r1", repo)
	base := filepath.Dir(first)
	for _, d := range []string{first, GenerationDir(base, "consult-r1", 2)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := GenerationDir(base, "consult-r1", 2)
	if ref, _ := ResolveRef("consult-r1", repo); ref != want {
		t.Fatalf("ResolveRef(name) = %q, want %q", ref, want)
	}
	if ref, _ := ResolveRef("consult-r1/codex", repo); ref != filepath.Join(want, "codex") {
		t.Fatalf("ResolveRef(name/member) = %q, want the member under %q", ref, want)
	}
	// The first generation stays an identity, reachable by its path.
	if ref, _ := ResolveRef(first, repo); ref != first {
		t.Fatalf("ResolveRef(path) = %q, want %q", ref, first)
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
	ws := Workspace{Dir: dir}
	if err := ws.Prepare([]byte("do the thing")); err != nil {
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

// A record round-trips, and a record from another engine version is refused
// by name rather than read as if its fields meant what this engine's do.
func TestMetaRoundTripAndSchemaRefusal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meta.json")
	m := &Meta{SchemaVersion: MetaSchemaVersion, Status: StatusOK, Provider: "codex", PromptState: PromptAccepted, ResultKind: ResultFinal}
	if err := m.WriteFile(p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMetaFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOK || got.Provider != "codex" || got.CollectedAt != nil {
		t.Fatalf("round trip = %+v", got)
	}
	if got.ReconciledAt != nil {
		t.Fatal("reconciledAt must stay absent until reconciliation")
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "reconciledAt") {
		t.Fatal("reconciledAt must be omitted when unset")
	}
	if err := os.WriteFile(p, []byte(`{"schemaVersion":8,"status":"ok","provider":"codex"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMetaFile(p); err == nil || !strings.Contains(err.Error(), "schema 8") {
		t.Fatalf("another schema must be refused by name, got %v", err)
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

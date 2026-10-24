package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// What a voice's '=' means on the command line. The grammar, the help pages
// and every refusal a caller reads are pinned in cmd/envoy, through the same
// entry main runs; this suite keeps what needs a built binary end to end.

// '=' splits a voice from its prompt file at the first occurrence, so a prompt
// path may contain one. The only spelling that could carry '=' on the voice
// side is a job named by a directory path, and that is refused by name rather
// than split into a job and a file the caller never meant.
func TestEqualsInPathsIsSplitOnceAndRefusedInJobPaths(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	src := t.TempDir()
	odd := filepath.Join(src, "a=b.md")
	os.WriteFile(odd, []byte("odd prompt body\n"), 0o644)

	outDir := filepath.Join(t.TempDir(), "job")
	res := runEnvoy(t, e, "run", outDir, "--with", "codex="+odd, "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if got := readFile(t, filepath.Join(outDir, "prompt.md")); got != "odd prompt body\n" {
		t.Fatalf("prompt.md = %q", got)
	}

	// A job that lives at a path with '=' cannot be continued by that path.
	oddJob := filepath.Join(t.TempDir(), "review=old")
	if r := runEnvoy(t, e, runArgs(odd, oddJob, "--with", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("odd job exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	r2 := filepath.Join(t.TempDir(), "round2")
	res = runEnvoy(t, e, runArgs(odd, r2, "--with", "@"+oddJob)...)
	if res.code != 3 {
		t.Fatalf("continuing a '=' path: exit = %d, want 3\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "a job directory path may not contain '='")
	if _, err := os.Stat(r2); !os.IsNotExist(err) {
		t.Fatal("a refused continuation must leave no job dir")
	}
}

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func jobLineOf(stdout string) string {
	for line := range strings.SplitSeq(stdout, "\n") {
		if after, ok := strings.CutPrefix(line, "job: "); ok {
			return after
		}
	}
	return ""
}

// A name keeps passing on, generation after generation, and a job that ended
// badly releases its name on collection exactly as an ok one does: delivery,
// not success, is what frees a name.
func TestANamePassesOnPastTheSecondGenerationAndAfterAFailure(t *testing.T) {
	ok := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	dispatch := func(e *env) runResult {
		return runEnvoyIn(t, e, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	}
	first := dispatch(ok)
	base := jobLineOf(first.stdout)
	if first.code != 0 || base == "" {
		t.Fatalf("first dispatch = %d\n%s", first.code, first.stderr)
	}
	runEnvoyIn(t, ok, project, "collect", "consult-r1")

	failing := &env{home: ok.home, extra: map[string]string{"ENVOY_FAKE_SCENARIO": "partial-failure"}}
	failed := runEnvoyIn(t, failing, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "claude", "--timeout-min", "5")
	if failed.code != 1 || jobLineOf(failed.stdout) != base+"+2" {
		t.Fatalf("second dispatch = %d in %q, want a failed turn in %s\n%s", failed.code, jobLineOf(failed.stdout), base+"+2", failed.stderr)
	}
	runEnvoyIn(t, ok, project, "collect", "consult-r1")

	third := dispatch(ok)
	if third.code != 0 || jobLineOf(third.stdout) != base+"+3" {
		t.Fatalf("third dispatch = %d in %q, want 0 in %s\n%s", third.code, jobLineOf(third.stdout), base+"+3", third.stderr)
	}
}

// A session held at dispatch refuses one fan-out member before it writes a
// record. Nothing can ever be collected for that member, so once the members
// that did run are delivered the name passes on — and while they are not, the
// refusal names the member that holds it rather than an "empty directory".
func TestARefusedFanMemberDoesNotPinTheName(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "sess-codex-held").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "sess-claude-free")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	if r := runEnvoyIn(t, e, project, "run", "pair", "--prompt-file", prompt, "--with", "codex", "--with", "claude:opus", "--timeout-min", "5"); r.code != 0 {
		t.Fatalf("round 1 = %d\n%s", r.code, r.stderr)
	}
	runEnvoyIn(t, e, project, "collect", "pair")

	lockDir := filepath.Join(e.home, ".local", "state", "envoy", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"pid":%d,"runnerInstanceId":"another-turn","outDir":"%s","startedAt":"2026-07-28T00:00:00.000Z"}`, os.Getpid(), t.TempDir())
	if err := os.WriteFile(filepath.Join(lockDir, "sess-codex-held.lock"), []byte(payload+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	round := func() runResult {
		return runEnvoyIn(t, e, project, "run", "pair-r2", "--prompt-file", prompt, "--with", "@pair", "--timeout-min", "5")
	}
	first := round()
	if first.code != 6 {
		t.Fatalf("round with one held member = %d, want 6\n%s", first.code, first.stderr)
	}
	r2 := jobLineOf(first.stdout)

	held := round()
	if held.code != 3 {
		t.Fatalf("dispatch under the uncollected round's name = %d, want 3\n%s", held.code, held.stderr)
	}
	mustContain(t, "stderr", held.stderr, "whose member claude-opus finished but has not been collected", "envoy collect '"+r2+"'")
	mustNotContain(t, "stderr", held.stderr, "empty directory", "holds no record")

	if r := runEnvoyIn(t, e, project, "collect", "pair-r2"); r.code != 0 {
		t.Fatalf("collect of the partial round = %d\n%s", r.code, r.stderr)
	}
	again := round()
	if again.code != 6 || jobLineOf(again.stdout) != r2+"+2" {
		t.Fatalf("after delivery the name must pass on: exit %d in %q, want 6 in %s\n%s", again.code, jobLineOf(again.stdout), r2+"+2", again.stderr)
	}
}

// A store that exists but cannot be listed is not an empty one: resolving a
// name there could serve an older generation as the latest, so collect and
// dispatch both stop instead.
func TestAnUnlistableStoreNeverServesAnOlderGeneration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	var store string
	for range 2 {
		r := runEnvoyIn(t, e, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
		if r.code != 0 {
			t.Fatalf("dispatch = %d\n%s", r.code, r.stderr)
		}
		store = filepath.Dir(jobLineOf(r.stdout))
		runEnvoyIn(t, e, project, "collect", "consult-r1")
	}
	// Traversable, not listable: generation 1 still opens by its bare path.
	if err := os.Chmod(store, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(store, 0o755) })

	col := runEnvoyIn(t, e, project, "collect", "--status-only", "consult-r1")
	if col.code != 2 {
		t.Fatalf("collect over an unlistable store = %d, want 2\nstdout:\n%s\nstderr:\n%s", col.code, col.stdout, col.stderr)
	}
	mustContain(t, "stderr", col.stderr, "could not be read", "not the same as an empty store")
	mustNotContain(t, "stdout", col.stdout, "status: ok")

	run := runEnvoyIn(t, e, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	if run.code != 2 {
		t.Fatalf("dispatch over an unlistable store = %d, want 2\n%s", run.code, run.stderr)
	}
	cont := runEnvoyIn(t, e, project, "run", "consult-r2", "--prompt-file", prompt, "--with", "@consult-r1", "--timeout-min", "5")
	if cont.code == 0 {
		t.Fatalf("a continuation must not resolve a name in an unlistable store\n%s", cont.stdout)
	}
	mustContain(t, "stderr", cont.stderr, "could not be read")
}

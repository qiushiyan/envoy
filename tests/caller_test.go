package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// as is the same machine and store seen by another caller: a harness session
// with its own identity, or — caller "" — one that exports none.
func (e *env) as(caller, codexSession string) *env {
	extra := map[string]string{}
	for k, v := range e.extra {
		extra[k] = v
	}
	delete(extra, "ENVOY_CALLER")
	if caller != "" {
		extra["ENVOY_CALLER"] = caller
	}
	if codexSession != "" {
		extra["ENVOY_FAKE_SESSION_ID_CODEX"] = codexSession
	}
	return &env{home: e.home, extra: extra}
}

// Two sessions in one checkout reach for the same name. Neither is refused,
// neither ever reads the other's job, and a name keeps meaning its caller's
// own job however the two interleave — including the continuation a caller
// makes by name long after the other session reused it.
func TestANameMeansItsCallersOwnJob(t *testing.T) {
	base := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	a, b := base.as("session-a", "sess-a"), base.as("session-b", "sess-b")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	run := func(e *env, name string, extra ...string) runResult {
		return runEnvoyIn(t, e, project, append([]string{"run", name, "--prompt-file", prompt, "--timeout-min", "5"}, extra...)...)
	}
	collectDir := func(e *env, ref string) string {
		r := runEnvoyIn(t, e, project, "collect", "--status-only", ref)
		if r.code != 0 {
			t.Fatalf("collect %s = %d\n%s", ref, r.code, r.stderr)
		}
		return jobLineOf(r.stdout)
	}

	first := run(a, "consult-r1", "--with", "codex")
	aDir := jobLineOf(first.stdout)
	if first.code != 0 || readMeta(t, aDir)["caller"] != "session-a" {
		t.Fatalf("A's dispatch = %d, caller %v", first.code, readMeta(t, aDir)["caller"])
	}

	// A has not collected. B is a different caller, so A's job does not hold
	// the name against it: B is not refused and A's name is not disturbed.
	second := run(b, "consult-r1", "--with", "codex")
	bDir := jobLineOf(second.stdout)
	if second.code != 0 || bDir != aDir+"+2" {
		t.Fatalf("B's dispatch beside A's uncollected job = %d in %q, want 0 in %s\n%s", second.code, bDir, aDir+"+2", second.stderr)
	}
	if got := collectDir(a, "consult-r1"); got != aDir {
		t.Fatalf("A's consult-r1 = %q, want its own %q", got, aDir)
	}
	if got := collectDir(b, "consult-r1"); got != bDir {
		t.Fatalf("B's consult-r1 = %q, want its own %q", got, bDir)
	}

	// A's own uncollected job still holds the name against A itself.
	if r := run(a, "consult-r1", "--with", "codex"); r.code != 3 {
		t.Fatalf("A re-dispatching over its own uncollected job = %d, want 3\n%s", r.code, r.stderr)
	}

	// Both deliver; then A continues its consult by name. It is A's
	// conversation that continues, not the newer one.
	runEnvoyIn(t, a, project, "collect", "consult-r1")
	runEnvoyIn(t, b, project, "collect", "consult-r1")
	if r := run(a, "review-r1", "--with", "@consult-r1"); r.code != 0 {
		t.Fatalf("A's continuation by name = %d\n%s", r.code, r.stderr)
	}
	review := readMeta(t, collectDir(a, "review-r1"))
	if review["resumedFrom"] != aDir || review["sessionId"] != "sess-a" {
		t.Fatalf("A's continuation resumed %v (session %v), want %s (sess-a)", review["resumedFrom"], review["sessionId"], aDir)
	}

	// A's next consult-r1 is generation 3, and each name still means its own.
	third := run(a, "consult-r1", "--with", "codex")
	if third.code != 0 || jobLineOf(third.stdout) != aDir+"+3" {
		t.Fatalf("A's second consult = %d in %q, want 0 in %s", third.code, jobLineOf(third.stdout), aDir+"+3")
	}
	if got := collectDir(b, "consult-r1"); got != bDir {
		t.Fatalf("B's consult-r1 after A's newer dispatch = %q, want still %q", got, bDir)
	}

	// A caller with no generation of its own — work picked up in a new
	// session — and one with no identity at all both read the newest.
	c, nobody := base.as("session-c", "sess-c"), base.as("", "")
	for _, e := range []*env{c, nobody} {
		if got := collectDir(e, "consult-r1"); got != aDir+"+3" {
			t.Fatalf("a caller with no generation of its own reads %q, want the newest %s", got, aDir+"+3")
		}
	}

	// Only the engine knows a name fell back, and it cannot tell work picked
	// up on purpose from a caller whose identity changed under it — so the
	// block says which happened, directly under the job it resolved to.
	picked := runEnvoyIn(t, c, project, "collect", "consult-r1")
	mustContain(t, "C's block", picked.stdout,
		"job: "+aDir+"+3\nnote: this session has dispatched no job named consult-r1", "another session dispatched it", "Check it is the job you mean")
	for who, r := range map[string]runResult{
		"its own job":          runEnvoyIn(t, b, project, "collect", "consult-r1"),
		"no identity":          runEnvoyIn(t, nobody, project, "collect", "consult-r1"),
		"a path":               runEnvoyIn(t, c, project, "collect", bDir),
		"its own continuation": runEnvoyIn(t, a, project, "collect", "review-r1"),
	} {
		mustNotContain(t, "a block read by "+who, r.stdout+r.stderr, "note:")
	}
	// The payload alone stays the payload: the note moves to stderr.
	payload := runEnvoyIn(t, c, project, "collect", "--result-only", "consult-r1")
	mustNotContain(t, "result-only stdout", payload.stdout, "note:")
	mustContain(t, "result-only stderr", payload.stderr, "note: this session has dispatched no job named consult-r1")

	// A continuation by a name that fell back continues another session's
	// conversation. The new job is the caller's own, so its block is where
	// that is said — from the two records, whoever reads it.
	if r := run(c, "pickup-r2", "--with", "@consult-r1"); r.code != 0 {
		t.Fatalf("C's continuation = %d\n%s", r.code, r.stderr)
	}
	mustContain(t, "C's continued block", runEnvoyIn(t, c, project, "collect", "pickup-r2").stdout,
		"resumed-from: "+aDir+"+3\nnote: this turn continued a conversation another session began")
}

// A caller without an identity shares the newest-of-anyone meaning of a name
// with everyone, so holds bind in both directions: it is refused while the
// newest job is undelivered, and its own undelivered job refuses others.
func TestACallerWithoutIdentityStillHoldsAndIsHeld(t *testing.T) {
	base := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	a, nobody := base.as("session-a", ""), base.as("", "")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	run := func(e *env, name string) runResult {
		return runEnvoyIn(t, e, project, "run", name, "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	}

	if r := run(a, "review-r1"); r.code != 0 {
		t.Fatalf("A's dispatch = %d\n%s", r.code, r.stderr)
	}
	if r := run(nobody, "review-r1"); r.code != 3 {
		t.Fatalf("an identity-less dispatch over A's uncollected job = %d, want 3\n%s", r.code, r.stderr)
	}

	held := run(nobody, "solo")
	if held.code != 0 {
		t.Fatalf("identity-less dispatch = %d\n%s", held.code, held.stderr)
	}
	if _, has := readMeta(t, jobLineOf(held.stdout))["caller"]; has {
		t.Fatal("a job dispatched with no identity must record no caller")
	}
	refused := run(a, "solo")
	if refused.code != 3 {
		t.Fatalf("A over an identity-less caller's uncollected job = %d, want 3\n%s", refused.code, refused.stderr)
	}
	mustContain(t, "stderr", refused.stderr, "nothing was dispatched", "has not been collected")
	runEnvoyIn(t, nobody, project, "collect", "solo")
	if r := run(a, "solo"); r.code != 0 {
		t.Fatalf("A once that job is delivered = %d\n%s", r.code, r.stderr)
	}
}

// The identity comes from the harness: Claude Code's exported session id, or
// ENVOY_CALLER, which any harness can set and which wins. A fan-out records
// it on the manifest and every member, and members resolve under the
// caller's own generation.
func TestCallerIdentityIsReadFromTheHarness(t *testing.T) {
	base := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	fan := func(e *env) string {
		r := runEnvoyIn(t, e, project, "run", "pair", "--prompt-file", prompt, "--with", "codex", "--with", "claude", "--timeout-min", "5")
		if r.code != 0 {
			t.Fatalf("fan-out = %d\n%s", r.code, r.stderr)
		}
		return jobLineOf(r.stdout)
	}

	claudeCode := base.as("", "")
	claudeCode.extra["CLAUDE_CODE_SESSION_ID"] = "cc-session"
	first := fan(claudeCode)
	if got := readGroup(t, first)["caller"]; got != "cc-session" {
		t.Fatalf("group caller = %v, want the Claude Code session id", got)
	}
	if got := readMeta(t, filepath.Join(first, "codex"))["caller"]; got != "cc-session" {
		t.Fatalf("member caller = %v, want the Claude Code session id", got)
	}

	explicit := base.as("explicit", "")
	explicit.extra["CLAUDE_CODE_SESSION_ID"] = "cc-session"
	second := fan(explicit)
	if got := readGroup(t, second)["caller"]; got != "explicit" || second != first+"+2" {
		t.Fatalf("ENVOY_CALLER must win and run beside the first: caller %v in %q", got, second)
	}

	for e, want := range map[*env]string{claudeCode: first, explicit: second} {
		r := runEnvoyIn(t, e, project, "collect", "--status-only", "pair/codex")
		if r.code != 0 || jobLineOf(r.stdout) != filepath.Join(want, "codex") {
			t.Fatalf("pair/codex = %q, want the member under %s\n%s", jobLineOf(r.stdout), want, r.stderr)
		}
	}
}

// The newest job is what a name means to every caller without a generation
// of its own, so it holds the name even against a caller whose own latest job
// under it is delivered: that dispatch would take the name from someone who
// may still collect by it.
func TestTheNewestJobHoldsTheNameForCallersWhoShareIt(t *testing.T) {
	base := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	a, nobody := base.as("session-a", ""), base.as("", "")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	run := func(e *env) runResult {
		return runEnvoyIn(t, e, project, "run", "solo", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	}
	if r := run(a); r.code != 0 {
		t.Fatalf("A's dispatch = %d\n%s", r.code, r.stderr)
	}
	runEnvoyIn(t, a, project, "collect", "solo")
	newest := run(nobody)
	if newest.code != 0 {
		t.Fatalf("identity-less dispatch over A's delivered job = %d\n%s", newest.code, newest.stderr)
	}
	refused := run(a)
	if refused.code != 3 {
		t.Fatalf("A over a newer identity-less uncollected job = %d, want 3\n%s", refused.code, refused.stderr)
	}
	mustContain(t, "stderr", refused.stderr, jobLineOf(newest.stdout), "has not been collected")
}

// A record that is there and cannot be read never lets an older job of the
// caller's answer to the name: collect and dispatch stop and name the record.
func TestAnUnreadableRecordNeverYieldsAnOlderGeneration(t *testing.T) {
	a := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").as("session-a", "")
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	var dirs []string
	for range 2 {
		r := runEnvoyIn(t, a, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
		if r.code != 0 {
			t.Fatalf("dispatch = %d\n%s", r.code, r.stderr)
		}
		dirs = append(dirs, jobLineOf(r.stdout))
		runEnvoyIn(t, a, project, "collect", "consult-r1")
	}
	if err := os.WriteFile(filepath.Join(dirs[1], "meta.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	col := runEnvoyIn(t, a, project, "collect", "--status-only", "consult-r1")
	if col.code != 2 {
		t.Fatalf("collect past an unreadable newer record = %d, want 2\nstdout:\n%s\nstderr:\n%s", col.code, col.stdout, col.stderr)
	}
	mustContain(t, "stderr", col.stderr, "was not resolved", dirs[1], "an older one must not answer for it")
	mustNotContain(t, "stdout", col.stdout, "status: ok")
	if r := runEnvoyIn(t, a, project, "run", "consult-r1", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5"); r.code != 2 {
		t.Fatalf("dispatch past an unreadable record = %d, want 2\n%s", r.code, r.stderr)
	}
	// The older job is still reachable, by the identity that cannot drift.
	if r := runEnvoyIn(t, a, project, "collect", "--status-only", dirs[0]); r.code != 0 || jobLineOf(r.stdout) != dirs[0] {
		t.Fatalf("collect by path = %d\n%s", r.code, r.stderr)
	}
}

// A dispatched turn is a session of its own. A job it dispatches through
// envoy does not answer to the dispatcher's names.
func TestADispatchedTurnDoesNotInheritItsDispatchersNames(t *testing.T) {
	a := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").as("session-a", "")
	a.extra["CLAUDE_CODE_SESSION_ID"] = "cc-session-a"
	project := t.TempDir()
	prompt := writePrompt(t, t.TempDir())
	own := runEnvoyIn(t, a, project, "run", "nested-source", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5")
	if own.code != 0 {
		t.Fatalf("A's dispatch = %d\n%s", own.code, own.stderr)
	}
	runEnvoyIn(t, a, project, "collect", "nested-source")

	outer := a.as("session-a", "")
	outer.extra["ENVOY_FAKE_NESTED_CMD"] = fmt.Sprintf("cd %q && %q run nested-source --prompt-file %q --with codex --timeout-min 5", project, binPath, prompt)
	if r := runEnvoyIn(t, outer, project, "run", "outer", "--prompt-file", prompt, "--with", "codex", "--timeout-min", "5"); r.code != 0 {
		t.Fatalf("outer dispatch = %d\n%s", r.code, r.stderr)
	}
	nested := jobLineOf(own.stdout) + "+2"
	if caller, has := readMeta(t, nested)["caller"]; has {
		t.Fatalf("the nested job recorded the dispatcher's identity %v", caller)
	}
	if r := runEnvoyIn(t, a, project, "collect", "--status-only", "nested-source"); jobLineOf(r.stdout) != jobLineOf(own.stdout) {
		t.Fatalf("A's nested-source = %q, want its own %q", jobLineOf(r.stdout), jobLineOf(own.stdout))
	}
}

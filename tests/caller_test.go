package integration

import (
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
	for _, e := range []*env{base.as("session-c", ""), base.as("", "")} {
		if got := collectDir(e, "consult-r1"); got != aDir+"+3" {
			t.Fatalf("a caller with no generation of its own reads %q, want the newest %s", got, aDir+"+3")
		}
	}
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

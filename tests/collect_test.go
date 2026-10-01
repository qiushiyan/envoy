package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Collection and pending discovery: the tiered block, delivery and its
// stamp, the selection flags, reconciliation, and the recovery index.

// A reported model that differs from the requested one is not evidence of a
// substitution: `opus` against `claude-opus-5` is the provider's own alias
// resolution, the mapping envoy refuses to own because it is the provider's to
// change. String inequality cannot tell that apart from a real substitution, so
// the block does not try — treating the difference as a surprise would be
// inferring, and would fire on the most ordinary claude dispatch there is. The
// observation stays in meta.json and --status-only, where it is provable.
func TestCollectOkBlockDoesNotGuessAtModelSubstitution(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude:opus",
		"--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if got := readMeta(t, outDir)["providerReportedModel"]; got != "fake-claude-model" {
		t.Fatalf("the reported model must still be recorded, got %v", got)
	}

	// The resume command legitimately carries `--model opus` — it replays the
	// dispatch — so the assertion is on the settings line itself.
	collected := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "collect stdout", collected.stdout, "status: ok")
	mustNotContain(t, "ok block", collected.stdout,
		"provider: claude · model", "fake-claude-model")

	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	mustContain(t, "status-only stdout", status.stdout,
		"provider: claude · model opus (ran fake-claude-model) · effort (provider default)")
}

func TestCollectStampsAndPendingDiscovers(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	base := t.TempDir()
	outDir := filepath.Join(base, "20260725-120000-consult")
	prompt := writePrompt(t, t.TempDir())

	if res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); res.code != 0 {
		t.Fatalf("turn failed: %d\n%s", res.code, res.stderr)
	}

	pending := runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending stdout", pending.stdout,
		"pending jobs: 1",
		"[terminal:ok] "+outDir,
		"terminal status ok has not been collected",
		"next: envoy collect")

	collected := runEnvoy(t, e, "collect", outDir)
	if collected.code != 0 {
		t.Fatalf("collect = %d\n%s", collected.code, collected.stderr)
	}
	mustContain(t, "collect stdout", collected.stdout,
		"job: "+outDir,
		"status: ok",
		"resume: envoy run <new-job-name> --with @'"+outDir+"' --timeout-min 5 --prompt-file <your-follow-up.md>",
		"--- result.md ---",
		"fake provider result",
		"next: result.md above is this turn's return value")

	meta := readMeta(t, outDir)
	if meta["collectedAt"] == nil {
		t.Fatal("collection must stamp collectedAt")
	}

	pending = runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending after collect", pending.stdout, "pending jobs: 0", "no recovery action is needed")
}

// The block is read by an agent whose context the result body competes for, so
// a turn that worked prints what the caller acts on and nothing else. Every
// field held back here answers a question only a turn that went wrong raises,
// and none of them is lost: meta.json is the record, and --status-only prints
// the whole preamble on demand.
func TestCollectOkBlockHoldsBackDiagnostics(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	if res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); res.code != 0 {
		t.Fatalf("turn failed: %d\n%s", res.code, res.stderr)
	}

	collected := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "ok block", collected.stdout,
		"job: "+outDir,
		"status: ok",
		"duration: ",
		"resume: envoy run <new-job-name> --with @'"+outDir+"'",
		"fake provider result")
	mustNotContain(t, "ok block", collected.stdout,
		"provider: codex",
		"tokens: ",
		"prompt: accepted",
		"result kind: ",
		"logs: progress ",
		// The continuation names the job, not the session; a bare id line
		// is one more chance to hand-assemble a follow-up out of it.
		"session: fake-session-id\n")

	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	mustContain(t, "status-only block", status.stdout,
		"status: ok",
		"provider: codex · model (provider default) · effort (provider default)",
		"tokens: input 13 · cachedInput 5 · output 8 · reasoningOutput 3",
		"prompt: accepted",
		"result kind: final",
		"logs: progress ",
		"next: this was a status check only")
	// Asking for the whole preamble asks for every field, not for the same
	// identifier three times: the bare line stays suppressed wherever a
	// command below already spells the session out.
	mustNotContain(t, "status-only block", status.stdout, "session: fake-session-id\n")
}

// The mirror: a turn that did not return a result keeps every diagnostic field,
// because its recovery prose reasons from them. The log paths in particular
// must survive — the next line names those three files by name, and a
// prescription may not point at paths the block withheld.
func TestCollectFailedBlockCarriesDiagnostics(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "nonzero-final")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())

	if res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); res.code != 1 {
		t.Fatalf("turn exit = %d, want 1\n%s", res.code, res.stderr)
	}

	collected := runEnvoy(t, e, "collect", outDir)
	mustContain(t, "failed block", collected.stdout,
		"status: failed — the provider ran and reported a failure",
		"provider: codex",
		"tokens: ",
		"prompt: accepted",
		"result kind: partial",
		"logs: progress "+filepath.Join(outDir, "progress.log"),
		"raw "+filepath.Join(outDir, "raw.log"),
		"stderr "+filepath.Join(outDir, "stderr.log"),
		"resume: envoy run <new-job-name> --with @'"+outDir+"'",
		"next: The provider accepted this prompt")
	// Even here the id is not repeated: the resume command carries it.
	mustNotContain(t, "failed block", collected.stdout, "session: fake-session-id\n")
}

// A store that could not be read is not an empty one. Reporting a mistyped
// --base as "no turn has run" — or, worse, as "no recovery action is needed" —
// states as fact something the engine never observed. A base that simply does
// not exist yet is the opposite case: that is exactly how a project looks
// before its first dispatch, and it must stay quiet and successful.
func TestPendingSeparatesAnUnreadableStoreFromAnEmptyOne(t *testing.T) {
	e := newEnv(t)

	// A base the caller *named* and got wrong raises the same "not found" as a
	// store awaiting its first dispatch, and the two mean opposite things. Only
	// the derived default may be waved through: a typo must refuse, or the
	// listing answers a question about a project the caller never asked about.
	absent := filepath.Join(t.TempDir(), "never-dispatched")
	for _, cmd := range []string{"pending"} {
		res := runEnvoy(t, e, cmd, "--base", absent)
		if res.code != 2 {
			t.Fatalf("%s on an explicit --base that does not exist must refuse, got exit %d\nstdout:\n%s", cmd, res.code, res.stdout)
		}
		mustContain(t, cmd+" stderr", res.stderr, "could not be read", "not the same as an empty store")
	}

	// The derived default store is created by the first dispatch, so its
	// absence is exactly how a project looks before it has ever run a turn.
	project := t.TempDir()
	for _, cmd := range []string{"pending"} {
		res := runEnvoyIn(t, e, project, cmd)
		if res.code != 0 {
			t.Fatalf("%s in a project that never dispatched is a normal first run: exit %d\n%s", cmd, res.code, res.stderr)
		}
	}

	sealed := filepath.Join(t.TempDir(), "sealed")
	if err := os.Mkdir(sealed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sealed, 0o755) })
	for _, cmd := range []string{"pending"} {
		res := runEnvoy(t, e, cmd, "--base", sealed)
		if res.code != 2 {
			t.Fatalf("%s over an unreadable store must fail as infra, got exit %d\nstdout:\n%s", cmd, res.code, res.stdout)
		}
		mustContain(t, cmd+" stderr", res.stderr, "could not be read", "not the same as an empty store")
		for _, forbidden := range []string{"no recovery action is needed"} {
			if strings.Contains(res.stdout, forbidden) {
				t.Fatalf("%s must not claim %q over a store it could not read:\n%s", cmd, forbidden, res.stdout)
			}
		}
	}
}

func TestCollectReconcilesAbandonedJob(t *testing.T) {
	e := newEnv(t)
	base := t.TempDir()
	outDir := filepath.Join(base, "20260725-130000-delegate")
	os.MkdirAll(outDir, 0o755)
	os.WriteFile(filepath.Join(outDir, "prompt.md"), []byte("x"), 0o644)
	meta := map[string]any{
		"schemaVersion": 10, "status": "running", "provider": "codex",
		"promptState": "accepted", "sessionId": "dead-session",
		"runnerPid": 4194304, "providerPid": 4194304, "providerPgid": 4194304,
		"timeoutMin": 180.0, "resultKind": "none",
		"collectedAt": nil,
	}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(outDir, "meta.json"), data, 0o644)

	// Discovery names the job and sends the caller to collect it: collection
	// is what reconciles the record and renders the recovery.
	pending := runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending stdout", pending.stdout, "[abandoned] "+outDir, "next: envoy collect '"+outDir+"'")

	res := runEnvoy(t, e, "collect", outDir)
	if res.code != 0 {
		t.Fatalf("collect = %d\n%s", res.code, res.stderr)
	}
	mustContain(t, "collect stdout", res.stdout,
		"status: abandoned — the process ended without publishing a result",
		"This turn ended without publishing a result",
		"continue the same session with a follow-up prompt",
		"envoy run <new-job-name> --with @'"+outDir+"' --timeout-min 180")
	got := readMeta(t, outDir)
	if got["status"] != "abandoned" || got["reconciledAt"] == nil {
		t.Fatalf("reconciled meta = status %v reconciledAt %v", got["status"], got["reconciledAt"])
	}
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")), "# Turn abandoned")
}

// --status-only reads coordinates without paying for the result body, and
// deliberately stamps nothing: the result has not been delivered, so pending
// discovery must keep listing the job.
func TestCollectStatusOnlyLeavesTheResultOwed(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	if r := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("turn exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	res := runEnvoy(t, e, "collect", "--status-only", outDir)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"status: ok",
		"resume: envoy run <new-job-name> --with @'"+outDir+"'",
		"next: this was a status check only",
	)
	for _, banned := range []string{"--- result.md ---", "fake provider result"} {
		if strings.Contains(res.stdout, banned) {
			t.Fatalf("status-only must not print %q:\n%s", banned, res.stdout)
		}
	}
	if readMeta(t, outDir)["collectedAt"] != nil {
		t.Fatal("status-only must not stamp collection")
	}
	// The full collect afterwards is still the first collection.
	if r := runEnvoy(t, e, "collect", outDir); r.code != 0 {
		t.Fatalf("full collect exit = %d", r.code)
	}
	if readMeta(t, outDir)["collectedAt"] == nil {
		t.Fatal("full collect must stamp collection")
	}
}

// --result-only hands back an ok turn's payload alone and stamps collection.
// A turn that is not ok prints its full block instead: its status and next
// action are its result, and suppressing them would hand back a payload that
// does not exist.
func TestCollectResultOnly(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	if r := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("turn exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	res := runEnvoy(t, e, "collect", "--result-only", outDir)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "fake provider result")
	for _, banned := range []string{"job:", "status:", "next:", "--- result.md ---"} {
		if strings.Contains(res.stdout, banned) {
			t.Fatalf("result-only must not print %q:\n%s", banned, res.stdout)
		}
	}
	if readMeta(t, outDir)["collectedAt"] == nil {
		t.Fatal("result-only delivered the result, so it must stamp collection")
	}

	// A failed turn has no payload to hand back alone.
	failDir := filepath.Join(t.TempDir(), "failed")
	e2 := newEnv(t).set("ENVOY_FAKE_SCENARIO", "nonzero-final")
	if r := runEnvoy(t, e2, runArgs(prompt, failDir, "--with", "codex", "--timeout-min", "5")...); r.code != 1 {
		t.Fatalf("failed turn exit = %d, want 1", r.code)
	}
	res = runEnvoy(t, e2, "collect", "--result-only", failDir)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "job: "+failDir, "status: failed", "next: ")

	// Both selectors at once select nothing coherent.
	res = runEnvoy(t, e, "collect", "--result-only", "--status-only", outDir)
	if res.code != 3 {
		t.Fatalf("exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "mutually exclusive")
}

// collectedAt means the result body reached a caller. A turn that reports ok
// while its result.md is unreadable delivered nothing — the collect must say
// so and leave the job owed, in both full and result-only modes.
func TestCollectDoesNotStampAnOkTurnWithoutItsResult(t *testing.T) {
	for _, args := range [][]string{{"collect"}, {"collect", "--result-only"}} {
		e := newEnv(t)
		dir := filepath.Join(t.TempDir(), "job")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := `{"schemaVersion":10,"status":"ok","provider":"codex","sessionId":"sess-1","promptState":"accepted","timeoutMin":5,"collectedAt":null}`
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}

		res := runEnvoy(t, e, append(args, dir)...)
		if res.code != 0 {
			t.Fatalf("%v exit = %d\nstderr:\n%s", args, res.code, res.stderr)
		}
		mustContain(t, fmt.Sprintf("stdout of %v", args), res.stdout, "result.md could not be read")
		// The next line printed here sends the caller to raw.log to recover the
		// payload, so the block must carry the paths: a prescription may not
		// point at files the block withheld. Status alone cannot decide this —
		// this turn is ok and still needs its logs, because its result never
		// reached anyone.
		mustContain(t, fmt.Sprintf("stdout of %v", args), res.stdout,
			"logs: progress "+filepath.Join(dir, "progress.log"),
			"raw "+filepath.Join(dir, "raw.log"),
			"stderr "+filepath.Join(dir, "stderr.log"))
		if readMeta(t, dir)["collectedAt"] != nil {
			t.Fatalf("%v stamped a result it never delivered", args)
		}
	}
}

// The option terminator is part of the documented grammar: a job whose
// spelling starts with a dash can only be named after `--`.
func TestCollectAcceptsTheOptionTerminator(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "job")
	prompt := writePrompt(t, t.TempDir())
	if r := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("turn exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	res := runEnvoy(t, e, "collect", "--status-only", "--", outDir)
	if res.code != 0 {
		t.Fatalf("collect -- <job>: exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "job: "+outDir, "status: ok")
}

// A prompt the provider never received is safe to send again — but only as
// the same dispatch. A continuation that failed to start must be re-sent into
// the same conversation with the same spend cap; a fresh cold voice with no
// budget is a different job, and offering it as "the identical retry" loses
// both the session and the safety net.
func TestRedispatchRepeatsTheDispatchItReplaces(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_SESSION_ID", "sess-r1")
	prompt := writePrompt(t, t.TempDir())
	r1 := filepath.Join(t.TempDir(), "consult")
	if r := runEnvoy(t, e, runArgs(prompt, r1, "--with", "claude:opus", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("round 1 exit = %d\nstderr:\n%s", r.code, r.stderr)
	}

	// No provider on PATH: the continuation provably never starts.
	noProvider := newEnv(t).set("PATH", t.TempDir())
	r2 := filepath.Join(t.TempDir(), "review")
	res := runEnvoy(t, noProvider, runArgs(prompt, r2, "--with", "@"+r1, "--max-budget-usd", "0.25", "--timeout-min", "9")...)
	if res.code != 2 {
		t.Fatalf("spawn failure exit = %d, want 2\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	// The caller's own file may be gone by the time the retry is read; the
	// command must point at the archive the job made, not the file it was given.
	if err := os.Remove(prompt); err != nil {
		t.Fatal(err)
	}
	col := runEnvoy(t, noProvider, "collect", r2)
	mustContain(t, "collect stdout", col.stdout,
		"never started",
		"envoy run <new-job-name>",
		"--with @'"+r1+"'",
		"--max-budget-usd 0.25",
		"--timeout-min 9",
		"--prompt-file '"+filepath.Join(r2, "prompt.md")+"'")
	mustNotContain(t, "collect stdout", col.stdout, "--with claude")
}

// agy, Google's Antigravity CLI, as a voice: its conversation, the standing
// instructions it reads as a rule from a folder added to its workspace, and
// the endings its stream can report — a continuation agy could not find, a
// stop it echoes as a result of its own, and a model selection it refuses
// before any conversation exists.
package integration

import (
	"path/filepath"
	"slices"
	"testing"
)

// A Carbon voice runs on the conversation agy names, and every launch of it —
// a continuation included — sends prompts as written, bypasses permission
// prompts and hands agy the job's own archive as the AGENTS.md of the folder
// it adds; the continuation resumes that conversation with the recorded
// model and effort.
func TestAgyConversationCarriesItsInstructionsAsARule(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "invocation.json")
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_SESSION_ID", "conv-carbon").
		set("ENVOY_FAKE_INVOCATION_FILE", capture)
	text := "Work unattended: nobody answers mid-turn.\n"
	rules := writeInstructions(t, text)
	prompt := writePrompt(t, t.TempDir())

	check := func(name, dir, stdout string) []string {
		t.Helper()
		inv := readInvocation(t, capture)
		if got := argAfter(inv.Argv, "--add-dir"); got != filepath.Join(dir, "agy-rules") {
			t.Fatalf("%s: --add-dir = %q, want the job's own rules folder", name, got)
		}
		if inv.AgyRule == nil || *inv.AgyRule != text {
			t.Fatalf("%s: agy read the rule %v, want the instructions", name, inv.AgyRule)
		}
		for _, flag := range []string{"--disable-slash-commands", "--dangerously-skip-permissions"} {
			if !slices.Contains(inv.Argv, flag) {
				t.Fatalf("%s: %s missing from %q", name, flag, inv.Argv)
			}
		}
		if got := argAfter(inv.Argv, "--model") + ":" + argAfter(inv.Argv, "--effort"); got != "gemini-4-carbon:high" {
			t.Fatalf("%s: model:effort = %s", name, got)
		}
		meta := readMeta(t, dir)
		if meta["sessionId"] != "conv-carbon" || meta["status"] != "ok" || meta["instructionsSha256"] != digest(text) {
			t.Fatalf("%s: session %v, status %v, digest %v", name, meta["sessionId"], meta["status"], meta["instructionsSha256"])
		}
		mustContain(t, name+" result", readFile(t, filepath.Join(dir, "result.md")), "fake provider result")
		return inv.Argv
	}

	r1 := filepath.Join(t.TempDir(), "consult")
	res := runEnvoy(t, e, runArgs(prompt, r1, "--with", "agy:gemini-4-carbon:high", "--instructions-file", rules)...)
	if res.code != 0 {
		t.Fatalf("first turn exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "first turn stdout", res.stdout, "provider: agy · model gemini-4-carbon · effort high", "session: conv-carbon")
	if argv := check("first turn", r1, res.stdout); slices.Contains(argv, "--conversation") {
		t.Fatalf("a fresh turn must not resume: %q", argv)
	}

	r2 := filepath.Join(t.TempDir(), "round2")
	res = runEnvoy(t, e, "run", r2, "--with", "@"+r1, "--prompt-file", prompt)
	if res.code != 0 {
		t.Fatalf("continuation exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	if got := argAfter(check("continuation", r2, res.stdout), "--conversation"); got != "conv-carbon" {
		t.Fatalf("continuation resumes %q, want conv-carbon", got)
	}
}

// agy answers a conversation it cannot find with a new one and exit 0. The
// turn is not the continuation asked for: it fails as such, its answer kept
// as partial output, and the follow-up it advertises continues the new
// conversation, which holds this turn.
func TestAgyReplacedConversationIsNotAContinuation(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_SESSION_ID", "conv-old")
	prompt := writePrompt(t, t.TempDir())
	r1 := filepath.Join(t.TempDir(), "consult")
	if res := runEnvoy(t, e, runArgs(prompt, r1, "--with", "agy")...); res.code != 0 {
		t.Fatalf("first turn exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	e.set("ENVOY_FAKE_SCENARIO", "conversation-replaced").set("ENVOY_FAKE_REPLACED_SESSION_ID", "conv-new")
	r2 := filepath.Join(t.TempDir(), "round2")
	res := runEnvoy(t, e, "run", r2, "--with", "@"+r1, "--prompt-file", prompt)
	if res.code != 1 {
		t.Fatalf("exit = %d, want 1 (failed)\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, r2)
	if meta["status"] != "failed" || meta["sessionId"] != "conv-new" || meta["promptState"] != "accepted" {
		t.Fatalf("status %v, session %v, prompt %v", meta["status"], meta["sessionId"], meta["promptState"])
	}
	if cause := meta["failure"].(map[string]any)["cause"]; cause != "session_not_continued" {
		t.Fatalf("cause = %v", cause)
	}
	col := runEnvoy(t, e, "collect", r2)
	mustContain(t, "collect", col.stdout,
		"Agy did not continue the conversation this turn resumed: it started a new one, session conv-new",
		`warning: conversation "conv-old" not found`,
		"a follow-up's prompt must carry whatever the earlier conversation established",
		"--with @'"+r2+"'",
		"fake provider result")
}

// A stop makes agy end its turn with a result of its own, ERROR
// "interrupted". The record is the stop envoy made — here the cap — with the
// acceptance the stream proved, never a provider verdict.
func TestAStoppedAgyTurnIsTheStopNotAVerdict(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "interrupted-on-term")
	prompt := writePrompt(t, t.TempDir())
	outDir := filepath.Join(t.TempDir(), "capped")
	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "agy", "--timeout-min", "0.02")...)
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4 (timeout)\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "timeout" || meta["promptState"] != "accepted" || meta["promptStateEvidence"] != "agy user_input" {
		t.Fatalf("status %v, prompt %v (%v)", meta["status"], meta["promptState"], meta["promptStateEvidence"])
	}
	if cause := meta["failure"].(map[string]any)["cause"]; cause != "timeout" {
		t.Fatalf("cause = %v", cause)
	}
	if tokens, _ := meta["tokens"].(map[string]any); tokens == nil || tokens["input"] != float64(40) {
		t.Fatalf("tokens = %v, want the fresh conversation's usage", meta["tokens"])
	}
	mustContain(t, "result.md", readFile(t, filepath.Join(outDir, "result.md")), "half an answer")
}

// A model selection agy refuses is refused before any conversation exists:
// the prompt never ran, and agy's own words name what to change.
func TestAgyRefusedModelNeverStarted(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "model-refused")
	prompt := writePrompt(t, t.TempDir())
	outDir := filepath.Join(t.TempDir(), "refused")
	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "agy:gemini-4-carbon:xhigh")...)
	if res.code != 1 {
		t.Fatalf("exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	meta := readMeta(t, outDir)
	if meta["status"] != "failed" || meta["promptState"] != "not_started" || meta["sessionId"] != nil {
		t.Fatalf("status %v, prompt %v, session %v", meta["status"], meta["promptState"], meta["sessionId"])
	}
	mustContain(t, "collect", runEnvoy(t, e, "collect", outDir).stdout,
		`gemini-4-carbon has no "xhigh" effort`, "The provider never started")
}

package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
)

func TestValidateEffort(t *testing.T) {
	if err := ValidateEffort("claude", ""); err != nil {
		t.Fatalf("empty effort must be valid: %v", err)
	}
	if err := ValidateEffort("codex", "ultra"); err != nil {
		t.Fatalf("codex ultra must be valid: %v", err)
	}
	err := ValidateEffort("claude", "minimal")
	if err == nil || !strings.Contains(err.Error(), "claude has no 'minimal'; its lowest is 'low'") {
		t.Fatalf("claude minimal must fail with the lowest-effort hint, got: %v", err)
	}
	err = ValidateEffort("claude", "ultra")
	if err == nil || !strings.Contains(err.Error(), "Valid: low, medium, high, xhigh, max") {
		t.Fatalf("claude ultra must list valid values, got: %v", err)
	}
	if err := ValidateEffort("gemini", "low"); err == nil {
		t.Fatal("unknown provider must fail")
	}
	// Effort travels in the voice (codex::high); there is no --effort flag to
	// send the caller looking for.
	if err := ValidateEffort("codex", "bogus"); err == nil || strings.Contains(err.Error(), "--effort") {
		t.Fatalf("the refusal must not name a flag that does not exist: %v", err)
	}
}

// claude runs headless and streaming, on the session envoy minted or the one
// it resumes — never both — with each setting passed only when it was set,
// permission prompts bypassed on every turn, and the standing instructions
// read from the job's own archive on every launch, a resume included.
func TestClaudeArgv(t *testing.T) {
	ws := job.Workspace{Dir: "/tmp/x"}
	fresh := newClaude(Options{}, ws, time.Now())
	if fresh.PreflightSessionID() == "" {
		t.Fatal("fresh claude must mint a session id before spawn")
	}
	expectArgv(t, "fresh", fresh.Argv(),
		map[string]string{"--output-format": "stream-json", "--session-id": fresh.PreflightSessionID(), "--permission-mode": "bypassPermissions"},
		[]string{"-p", "--verbose"},
		[]string{"--resume", "--model", "--effort", "--max-budget-usd", "--append-system-prompt-file"})

	budget := 0.5
	resumed := newClaude(Options{Model: "opus", Effort: "xhigh", Resume: "abc", MaxBudgetUSD: &budget, Instructions: "stay read-only\n"}, ws, time.Now())
	if got := resumed.PreflightSessionID(); got != "abc" {
		t.Fatalf("resume session id = %q, want abc", got)
	}
	expectArgv(t, "resumed", resumed.Argv(),
		map[string]string{"--output-format": "stream-json", "--resume": "abc", "--model": "opus", "--effort": "xhigh",
			"--max-budget-usd": "0.5", "--permission-mode": "bypassPermissions", "--append-system-prompt-file": ws.InstructionsPath()},
		[]string{"-p", "--verbose"},
		[]string{"--session-id"})
}

// codex runs `exec`, or `exec resume <thread>`, with its JSONL stream and the
// prompt on stdin (the trailing "-"), its last message written to the job's
// own file, each setting only when it was set, no git-root gate, and the
// standing instructions as a TOML string on every launch. No sandbox or
// approval flag ever: codex's own configuration governs.
func TestCodexArgv(t *testing.T) {
	ws := job.Workspace{Dir: "/tmp/x"}
	sandbox := []string{"--sandbox", "-s", "--full-auto", "--dangerously-bypass-approvals-and-sandbox"}

	fresh := newCodex(Options{}, ws)
	if fresh.PreflightSessionID() != "" {
		t.Fatal("fresh codex must not know a session id before spawn")
	}
	argv := fresh.Argv()
	if argv[0] != "exec" || argv[1] == "resume" || argv[len(argv)-1] != "-" {
		t.Fatalf("fresh argv = %v, want exec … -", argv)
	}
	expectArgv(t, "fresh", argv, map[string]string{"-o": ws.LastMessagePath()},
		[]string{"--json", "--skip-git-repo-check"}, append([]string{"-m", "-c"}, sandbox...))

	resumed := newCodex(Options{Model: "gpt-5.3", Effort: "xhigh", Resume: "tid", Instructions: "stay read-only\n"}, ws)
	argv = resumed.Argv()
	if !slices.Equal(argv[:2], []string{"exec", "resume"}) || !slices.Equal(argv[len(argv)-2:], []string{"tid", "-"}) {
		t.Fatalf("resume argv = %v, want exec resume … tid -", argv)
	}
	expectArgv(t, "resumed", argv, map[string]string{"-m": "gpt-5.3", "-o": ws.LastMessagePath()},
		[]string{"--json", "--skip-git-repo-check"}, sandbox)
	var configs []string
	for i, a := range argv[:len(argv)-1] {
		if a == "-c" {
			configs = append(configs, argv[i+1])
		}
	}
	if want := []string{"model_reasoning_effort=xhigh", `developer_instructions="stay read-only\n"`}; !slices.Equal(configs, want) {
		t.Fatalf("-c settings = %q, want %q", configs, want)
	}
}

// expectArgv checks argv by what a CLI parses — each flag's value, the flags
// that must be there and the ones that must not — never by position.
func expectArgv(t *testing.T, name string, argv []string, values map[string]string, present, absent []string) {
	t.Helper()
	for flag, want := range values {
		i := slices.Index(argv, flag)
		if i < 0 || i+1 >= len(argv) || argv[i+1] != want {
			t.Errorf("%s: %s must carry %q in %q", name, flag, want, argv)
		}
	}
	for _, flag := range present {
		if !slices.Contains(argv, flag) {
			t.Errorf("%s: %s missing from %q", name, flag, argv)
		}
	}
	for _, flag := range absent {
		if slices.Contains(argv, flag) {
			t.Errorf("%s: %s must not be passed: %q", name, flag, argv)
		}
	}
}

// codex parses the -c value as TOML, so the text must arrive as a string that
// decodes to exactly the bytes archived — quotes, backslashes, newlines and
// control characters included, and a text that would itself parse as another
// TOML type (true, 12, [a]) kept a string.
func TestTOMLStringRoundTrips(t *testing.T) {
	for _, s := range []string{
		"plain", "true", "12", "[a]", "a = b",
		"quote \" and backslash \\ here",
		"lines\nand\ttabs\r\nand a form feed\f and a bell\a",
		"nul \x00 and del \x7f",
		"unicode: 中文 — ✓",
	} {
		got := tomlString(s)
		if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
			t.Fatalf("tomlString(%q) = %s, want a basic string", s, got)
		}
		// TOML's basic-string escapes are a subset of Go's, so a Go unquote
		// reads the value back the way codex's parser does.
		back, err := strconv.Unquote(got)
		if err != nil || back != s {
			t.Fatalf("tomlString(%q) = %s, decodes to %q (%v)", s, got, back, err)
		}
		if strings.ContainsFunc(got, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			t.Fatalf("tomlString(%q) = %q carries a raw control character", s, got)
		}
		// Go's unquote also accepts escapes TOML lacks (\a, \x07), so the
		// round trip alone cannot tell a valid TOML string from a Go one.
		body := got[1 : len(got)-1]
		for i := 0; i < len(body); i++ {
			if body[i] != '\\' {
				continue
			}
			if i+1 == len(body) || !strings.ContainsRune(`btnfr"\u`, rune(body[i+1])) {
				t.Fatalf("tomlString(%q) = %s uses an escape TOML does not define", s, got)
			}
			i++
		}
	}
	if got := tomlString("bell\a nul\x00 del\x7f"); got != `"bell\u0007 nul\u0000 del\u007F"` {
		t.Fatalf("control characters = %s, want TOML's \\uXXXX spelling", got)
	}
}

func TestClaudeStreamParsing(t *testing.T) {
	c := newClaude(Options{}, job.Workspace{}, time.Now())

	// init reports the model and is a milestone; it proves the process
	// launched, never that the turn reached model work.
	events := c.Feed(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5"}`)
	if !slices.ContainsFunc(events, func(e Event) bool { return e.Kind == KindModelReported && e.Model == "claude-opus-5" }) ||
		!slices.ContainsFunc(events, func(e Event) bool { return e.Kind == KindNote && e.State == "provider-initialized" }) {
		t.Fatalf("init events = %+v, want the reported model and the initialized note", events)
	}
	if slices.ContainsFunc(events, func(e Event) bool { return e.Kind == KindAccepted }) {
		t.Fatalf("init must not prove acceptance: %+v", events)
	}

	// An init without a model field must not fabricate a report.
	quiet := newClaude(Options{}, job.Workspace{}, time.Now())
	for _, e := range quiet.Feed(`{"type":"system","subtype":"init","session_id":"s1"}`) {
		if e.Kind == KindModelReported {
			t.Fatal("no model in init must mean no KindModelReported event")
		}
	}

	events = c.Feed(`{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}`)
	foundAccepted := false
	for _, e := range events {
		if e.Kind == KindAccepted && e.Evidence == "claude assistant" {
			foundAccepted = true
		}
	}
	if !foundAccepted {
		t.Fatalf("assistant event must prove acceptance: %+v", events)
	}

	events = c.Feed(`{"type":"result","subtype":"success","is_error":false,"session_id":"s2","result":"done","total_cost_usd":0.02,"usage":{"input_tokens":10,"cache_read_input_tokens":3,"output_tokens":5}}`)
	foundTerminal := false
	for _, e := range events {
		if e.Kind == KindTerminal && e.Terminal == "claude success" {
			foundTerminal = true
		}
	}
	if !foundTerminal {
		t.Fatalf("result event must be terminal: %+v", events)
	}

	out := c.Conclude(ExitInfo{Code: new(0)})
	if out.Status != job.StatusOK || out.Text != "done" {
		t.Fatalf("outcome = %+v", out)
	}
	// A resumed conversation continues under a fresh id: the envelope's wins
	// over the one init announced.
	if out.SessionID != "s2" {
		t.Fatalf("envelope session id must win, got %q", out.SessionID)
	}
}

func TestClaudeFailureExcludesErrorEcho(t *testing.T) {
	c := newClaude(Options{}, job.Workspace{}, time.Now())
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"real partial work"}]}}`)
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"boom"}]}}`)
	c.Feed(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","errors":["boom"]}`)

	out := c.Conclude(ExitInfo{Code: new(7)})
	if out.Status != job.StatusFailed {
		t.Fatalf("status = %s", out.Status)
	}
	if out.Failure.Cause != job.CauseProviderVerdict || job.Deref(out.Failure.Message) != "boom" {
		t.Fatalf("failure = %+v", out.Failure)
	}
	if out.Partial == nil || *out.Partial != "real partial work" {
		t.Fatalf("partial must keep real work and drop the error echo, got %v", out.Partial)
	}
}

// A safety-classifier refusal reaches the result envelope as an ordinary
// error, worded for a person at a terminal. The stream names it twice before
// that — a system record and the synthetic assistant message that ends the
// turn — and the record keeps what those say: the category and the provider's
// explanation, as a cause of their own.
func TestClaudeRefusalIsItsOwnCause(t *testing.T) {
	const (
		explanation = "This request was blocked as it seems to violate the provider's restrictions on duplicating model outputs."
		terminal    = "API Error: Opus 5.5's safeguards flagged this message. Details: `[reasoning_extraction]`"
		notice      = `{"type":"user","isSynthetic":true,"message":{"role":"user","content":[{"type":"text","text":"Your response above was stopped by a safety classifier."}]}}`
		system      = `{"type":"system","subtype":"model_refusal_no_fallback","original_model":"claude-opus-5-5","api_refusal_category":"reasoning_extraction","api_refusal_explanation":"` + explanation + `"}`
		synthetic   = `{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","stop_reason":"refusal","stop_details":{"type":"refusal","category":"reasoning_extraction","explanation":"` + explanation + `"},"content":[{"type":"text","text":"` + terminal + `"}]},"error":"invalid_request"}`
		envelope    = `{"type":"result","subtype":"success","is_error":true,"stop_reason":"refusal","terminal_reason":"api_error","session_id":"s1","result":"` + terminal + `"}`
	)
	cases := []struct {
		name  string
		lines []string
	}{
		{"both records", []string{notice, system, synthetic, envelope}},
		{"the system record alone", []string{system, envelope}},
		{"the synthetic message alone", []string{synthetic, envelope}},
	}
	for _, tc := range cases {
		c := newClaude(Options{}, job.Workspace{}, time.Now())
		c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"work before the refusal"}]}}`)
		for _, line := range tc.lines {
			c.Feed(line)
		}
		out := c.Conclude(ExitInfo{Code: new(1)})
		if out.Status != job.StatusFailed || out.PromptState != job.PromptAccepted {
			t.Fatalf("%s: status %s, prompt state %s", tc.name, out.Status, out.PromptState)
		}
		f := out.Failure
		if f.Cause != job.CauseProviderRefusal || job.Deref(f.Code) != "reasoning_extraction" || job.Deref(f.Message) != explanation {
			t.Fatalf("%s: failure = %+v", tc.name, f)
		}
		if out.Partial == nil || *out.Partial != "work before the refusal" {
			t.Fatalf("%s: partial must keep the work and drop the terminal message, got %v", tc.name, out.Partial)
		}
	}

	// An error envelope with no refusal in the stream stays a verdict, even
	// when its words mention one: the cause is what the stream named.
	c := newClaude(Options{}, job.Workspace{}, time.Now())
	c.Feed(envelope)
	if f := c.Conclude(ExitInfo{Code: new(1)}).Failure; f.Cause != job.CauseProviderVerdict || job.Deref(f.Message) != terminal {
		t.Fatalf("an unnamed refusal must stay a verdict, got %+v", f)
	}
}

// The echo is left out of every source work is recovered from: a failed turn
// whose only assistant text repeats the error recovered nothing, whether the
// stream or the session transcript holds that text, and work the transcript
// keeps beside the echo is recovered without it.
func TestClaudeFailureExcludesErrorEchoFromTheTranscript(t *testing.T) {
	cases := []struct {
		name       string
		transcript []string
		want       string
	}{
		{"echo only", []string{"boom"}, ""},
		{"work beside the echo", []string{"real work", "boom"}, "real work"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", config)
			writeClaudeTranscript(t, config, "s1", tc.transcript...)
			c := newClaude(Options{}, job.Workspace{}, time.Now().Add(-time.Minute))
			c.Feed(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","errors":["boom"]}`)

			out := c.Conclude(ExitInfo{Code: new(1)})
			if got := job.Deref(out.Partial); got != tc.want {
				t.Fatalf("partial = %q, want %q", got, tc.want)
			}
			if out.PromptState != job.PromptAccepted || out.Evidence != "claude session transcript" {
				t.Fatalf("the transcript proves acceptance whatever text survives: %s by %q", out.PromptState, out.Evidence)
			}
		})
	}
}

// Recovered work keeps every assistant message as its own paragraph, with
// one message's text blocks run together as the provider split them.
func TestClaudeRecoversWorkAcrossMessages(t *testing.T) {
	c := newClaude(Options{}, job.Workspace{}, time.Now())
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first"}]}}`)
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second "},{"type":"tool_use","name":"Read"},{"type":"text","text":"half"}]}}`)
	c.Feed(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","errors":["boom"]}`)

	out := c.Conclude(ExitInfo{Code: new(1)})
	if got := job.Deref(out.Partial); got != "first\n\nsecond half" {
		t.Fatalf("partial = %q", got)
	}
}

// A verdict the provider gave no words for is recorded as exactly that: the
// provider's own code where it gave one, and no message the engine made up.
func TestAVerdictWithoutWordsRecordsNoMessage(t *testing.T) {
	c := newClaude(Options{}, job.Workspace{}, time.Now())
	c.Feed(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1"}`)
	if f := c.Conclude(ExitInfo{Code: new(1)}).Failure; f.Cause != job.CauseProviderVerdict || f.Message != nil ||
		job.Deref(f.Code) != "error_during_execution" {
		t.Fatalf("claude failure = %+v", f)
	}

	x := newCodex(Options{}, job.Workspace{Dir: t.TempDir()})
	x.Feed(`{"type":"thread.started","thread_id":"tid"}`)
	x.Feed(`{"type":"turn.failed"}`)
	if f := x.Conclude(ExitInfo{Code: new(1)}).Failure; f.Cause != job.CauseProviderVerdict || f.Message != nil {
		t.Fatalf("codex failure = %+v", f)
	}
}

// writeClaudeTranscript writes a session transcript under config holding one
// assistant message per text, stamped now.
func writeClaudeTranscript(t *testing.T, config, sessionID string, texts ...string) {
	t.Helper()
	dir := filepath.Join(config, "projects", "-tmp-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, text := range texts {
		record, _ := json.Marshal(map[string]any{
			"type":      "assistant",
			"timestamp": time.Now().Format(time.RFC3339),
			"message":   map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}},
		})
		lines = append(lines, string(record))
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeBudgetStop(t *testing.T) {
	budget := 0.25
	c := newClaude(Options{MaxBudgetUSD: &budget}, job.Workspace{}, time.Now())
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial"}]}}`)
	c.Feed(`{"type":"result","subtype":"error_max_budget_usd","session_id":"s1","usage":{"input_tokens":1}}`)

	out := c.Conclude(ExitInfo{Code: new(1)})
	if out.Status != job.StatusFailed || out.Failure.Cause != job.CauseBudgetCap {
		t.Fatalf("budget outcome = %+v", out)
	}
	// The driver states the cause; its wording, the cause-specific fix and
	// the recovery prescription are prose's, keyed off the record.
	if out.PromptState != job.PromptAccepted {
		t.Fatalf("a budget stop happens after acceptance, got %q", out.PromptState)
	}
}

func TestClaudeArrayAndNoiseTolerance(t *testing.T) {
	c := newClaude(Options{}, job.Workspace{}, time.Now())
	if events := c.Feed("not json at all"); events != nil {
		t.Fatalf("noise must yield no events, got %+v", events)
	}
	events := c.Feed(`[{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"a"}]}},{"type":"user"}]`)
	activity := 0
	for _, e := range events {
		if e.Kind == KindActivity {
			activity++
		}
	}
	if activity != 2 {
		t.Fatalf("array must record one activity per object, got %d", activity)
	}
}

func TestCodexFailureAndRecovery(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Options{}, ws)
	c.Feed(`{"type":"thread.started","thread_id":"tid-3"}`)
	c.Feed(`{"type":"turn.failed","error":{"message":"model exploded"}}`)

	out := c.Conclude(ExitInfo{Code: new(1)})
	if out.Status != job.StatusFailed || job.Deref(out.Failure.Message) != "model exploded" {
		t.Fatalf("outcome = %+v", out)
	}
	if out.PromptState != job.PromptAccepted {
		t.Fatalf("thread.started must mean accepted, got %s", out.PromptState)
	}

	ev := c.Recovery()
	if !ev.Accepted || ev.Label != "codex thread.started" {
		t.Fatalf("recovery = %+v", ev)
	}
}

// A verdict over a turn that left output proves acceptance by that output,
// and says so: an accepted prompt state never stands without its evidence.
func TestCodexFailureWithOutputButNoThreadNamesItsEvidence(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Options{}, ws)
	c.Feed(`{"type":"item.completed","item":{"type":"agent_message","text":"half"}}`)
	c.Feed(`{"type":"turn.failed","error":{"message":"model exploded"}}`)
	out := c.Conclude(ExitInfo{Code: new(1)})
	if out.PromptState != job.PromptAccepted || out.Evidence != "codex recovered output" || out.Partial == nil || *out.Partial != "half" {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestCodexNoResultUsesStderrTail(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Options{}, ws)
	out := c.Conclude(ExitInfo{Code: new(23), StderrTail: "a\n\nb\nc\nd\n"})
	if out.Status != job.StatusInfra {
		t.Fatalf("status = %s", out.Status)
	}
	if got := out.Failure.StderrTail; out.Failure.Cause != job.CauseExitedWithoutResult || strings.Join(got, "|") != "b|c|d" {
		t.Fatalf("stderr detail must keep the last three non-empty lines: %+v", out.Failure)
	}
	if out.PromptState != job.PromptUnknown {
		t.Fatalf("prompt state = %s", out.PromptState)
	}
}

// With no verdict and no result, the last transient error is detail on the
// infra outcome — the caller reads what the provider last said.
func TestCodexTransientErrorIsDetailWithoutResult(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Options{}, ws)
	c.Feed(`{"type":"thread.started","thread_id":"tid-5"}`)
	c.Feed(`{"type":"error","message":"Reconnecting... 5/5 (stream disconnected before completion)"}`)
	out := c.Conclude(ExitInfo{Code: new(1), StderrTail: "tail"})
	if out.Status != job.StatusInfra || !strings.Contains(job.Deref(out.Failure.LastErrorEvent), "stream disconnected") {
		t.Fatalf("outcome = %+v", out)
	}
}

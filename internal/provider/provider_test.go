package provider

import (
	"reflect"
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
}

func TestClaudeArgv(t *testing.T) {
	fresh := newClaude(Options{}, time.Now())
	argv := fresh.Argv()
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--session-id", fresh.sessionID,
		"--permission-mode", "bypassPermissions"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("fresh argv = %v, want %v", argv, want)
	}
	if fresh.PreflightSessionID() == "" {
		t.Fatal("fresh claude must mint a session id before spawn")
	}

	budget := 0.5
	full := newClaude(Options{
		Model: "opus", Effort: "xhigh",
		Resume: "abc", MaxBudgetUSD: &budget,
	}, time.Now())
	argv = full.Argv()
	want = []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--model", "opus", "--effort", "xhigh", "--resume", "abc",
		"--permission-mode", "bypassPermissions", "--max-budget-usd", "0.5",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("full argv = %v, want %v", argv, want)
	}
	if got := full.PreflightSessionID(); got != "abc" {
		t.Fatalf("resume session id = %q, want abc", got)
	}
	if env := full.ExtraEnv(); len(env) != 1 || env[0] != "API_FORCE_IDLE_TIMEOUT=1" {
		t.Fatalf("claude env = %v", env)
	}
}

func TestCodexArgv(t *testing.T) {
	ws := job.Workspace{Dir: "/tmp/x"}

	fresh := newCodex(Options{}, ws)
	argv := fresh.Argv()
	want := []string{"exec", "--json", "-o", ws.LastMessagePath(), "-"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("fresh argv = %v, want %v", argv, want)
	}
	if fresh.PreflightSessionID() != "" {
		t.Fatal("fresh codex must not know a session id before spawn")
	}

	resumed := newCodex(Options{Model: "gpt-5.3", Effort: "xhigh", Resume: "tid"}, ws)
	argv = resumed.Argv()
	want = []string{
		"exec", "resume", "--json", "-m", "gpt-5.3",
		"-c", "model_reasoning_effort=xhigh", "-o", ws.LastMessagePath(), "tid", "-",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("resume argv = %v, want %v", argv, want)
	}
}

func TestClaudeStreamParsing(t *testing.T) {
	c := newClaude(Options{}, time.Now())

	events := c.Feed(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5"}`)
	if len(events) != 4 || events[1].Kind != KindModelReported || events[1].Model != "claude-opus-5" ||
		events[2].Kind != KindNote || events[2].State != "provider-initialized" {
		t.Fatalf("init events = %+v", events)
	}

	// An init without a model field must not fabricate a report.
	quiet := newClaude(Options{}, time.Now())
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

	out := c.Conclude(ExitInfo{Code: job.Ptr(0)})
	if out.Status != job.StatusOK || out.Text != "done" {
		t.Fatalf("outcome = %+v", out)
	}
	if out.SessionID != "s2" {
		t.Fatalf("envelope session id must win, got %q", out.SessionID)
	}
	if out.Tokens == nil || *out.Tokens.Input != 10 || *out.Tokens.CacheRead != 3 || *out.Tokens.CacheCreation != 0 || *out.Tokens.Output != 5 {
		t.Fatalf("tokens = %+v", out.Tokens)
	}
	if out.CostUSD == nil || *out.CostUSD != 0.02 {
		t.Fatalf("cost = %v", out.CostUSD)
	}
}

func TestClaudeFailureExcludesErrorEcho(t *testing.T) {
	c := newClaude(Options{}, time.Now())
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"real partial work"}]}}`)
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"boom"}]}}`)
	c.Feed(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","errors":["boom"]}`)

	out := c.Conclude(ExitInfo{Code: job.Ptr(7)})
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

func TestClaudeBudgetStop(t *testing.T) {
	budget := 0.25
	c := newClaude(Options{MaxBudgetUSD: &budget}, time.Now())
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial"}]}}`)
	c.Feed(`{"type":"result","subtype":"error_max_budget_usd","session_id":"s1","usage":{"input_tokens":1}}`)

	out := c.Conclude(ExitInfo{Code: job.Ptr(1)})
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
	c := newClaude(Options{}, time.Now())
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

func TestCodexLifecycle(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Options{}, ws)

	events := c.Feed(`{"type":"thread.started","thread_id":"tid-1"}`)
	var kinds []EventKind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if !reflect.DeepEqual(kinds, []EventKind{KindActivity, KindSessionStarted, KindAccepted}) {
		t.Fatalf("thread.started events = %+v", events)
	}

	c.Feed(`{"type":"item.completed","item":{"type":"agent_message","text":"the answer"}}`)
	events = c.Feed(`{"type":"turn.completed","usage":{"input_tokens":13,"cached_input_tokens":5,"output_tokens":8,"reasoning_output_tokens":3}}`)
	if events[len(events)-1].Terminal != "codex turn.completed" {
		t.Fatalf("turn.completed events = %+v", events)
	}

	out := c.Conclude(ExitInfo{Code: job.Ptr(0)})
	if out.Status != job.StatusOK || out.Text != "the answer" {
		t.Fatalf("outcome = %+v", out)
	}
	if *out.Tokens.Input != 13 || *out.Tokens.CachedInput != 5 || *out.Tokens.ReasoningOutput != 3 {
		t.Fatalf("tokens = %+v", out.Tokens)
	}
}

func TestCodexTerminalEnvelopeWinsOverKill(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Options{}, ws)
	c.Feed(`{"type":"thread.started","thread_id":"tid-2"}`)
	c.Feed(`{"type":"item.completed","item":{"type":"agent_message","text":"finished work"}}`)
	c.Feed(`{"type":"turn.completed","usage":{"input_tokens":1}}`)

	// Killed during residual cleanup: no exit code, but the envelope landed.
	out := c.Conclude(ExitInfo{Terminated: true, TerminalType: "codex turn.completed"})
	if out.Status != job.StatusOK || out.Text != "finished work" {
		t.Fatalf("terminal envelope must win over the kill, got %+v", out)
	}
}

func TestCodexFailureAndRecovery(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Options{}, ws)
	c.Feed(`{"type":"thread.started","thread_id":"tid-3"}`)
	c.Feed(`{"type":"turn.failed","error":{"message":"model exploded"}}`)

	out := c.Conclude(ExitInfo{Code: job.Ptr(1)})
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
	out := c.Conclude(ExitInfo{Code: job.Ptr(1)})
	if out.PromptState != job.PromptAccepted || out.Evidence != "codex recovered output" || out.Partial == nil || *out.Partial != "half" {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestCodexNoResultUsesStderrTail(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Options{}, ws)
	out := c.Conclude(ExitInfo{Code: job.Ptr(23), StderrTail: "a\n\nb\nc\nd\n"})
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

// A transient `error` event is an observation, not a verdict: codex emits
// one per reconnect attempt and carries on, and a turn that then completes is
// a success. Only turn.failed fails a turn. (A real job once recorded
// `failed` over a full result because the reconnect messages overwrote the
// error text — EVIDENCE.md 2026-08-28.)
func TestCodexTransientErrorDoesNotOverrideCompletion(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Options{}, ws)
	c.Feed(`{"type":"thread.started","thread_id":"tid-4"}`)
	events := c.Feed(`{"type":"error","message":"Reconnecting... waiting for network (Connection failed: error sending request)"}`)
	var conn int
	for _, e := range events {
		if e.Kind == KindConnectionError {
			conn++
		}
	}
	if conn != 1 {
		t.Fatalf("a reconnect message must surface as one connection-error observation, got %+v", events)
	}
	if events := c.Feed(`{"type":"error","message":"some other transient thing"}`); len(events) != 1 || events[0].Kind != KindActivity {
		t.Fatalf("an unrecognized error is activity only, got %+v", events)
	}
	c.Feed(`{"type":"item.completed","item":{"type":"agent_message","text":"the answer"}}`)
	c.Feed(`{"type":"turn.completed","usage":{"input_tokens":1}}`)

	out := c.Conclude(ExitInfo{Code: job.Ptr(0)})
	if out.Status != job.StatusOK || out.Text != "the answer" {
		t.Fatalf("turn.completed with a result must win over earlier transient errors, got %+v", out)
	}
}

// With no verdict and no result, the last transient error is detail on the
// infra outcome — the caller reads what the provider last said.
func TestCodexTransientErrorIsDetailWithoutResult(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Options{}, ws)
	c.Feed(`{"type":"thread.started","thread_id":"tid-5"}`)
	c.Feed(`{"type":"error","message":"Reconnecting... 5/5 (stream disconnected before completion)"}`)
	out := c.Conclude(ExitInfo{Code: job.Ptr(1), StderrTail: "tail"})
	if out.Status != job.StatusInfra || !strings.Contains(job.Deref(out.Failure.LastErrorEvent), "stream disconnected") {
		t.Fatalf("outcome = %+v", out)
	}
}

// An ending the provider never concluded may claim acceptance only on the
// evidence's word, and always records how that was proven — or that it wasn't.
func TestEvidenceOutcomeTakesPromptStateFromAcceptance(t *testing.T) {
	partial := "half an answer"
	accepted := Evidence{Accepted: true, Label: "codex thread.started", Partial: &partial}.Outcome(job.StatusTimeout, job.Failure{Cause: job.CauseTimeout})
	if accepted.Status != job.StatusTimeout || accepted.Failure.Cause != job.CauseTimeout || accepted.Partial != &partial ||
		accepted.PromptState != job.PromptAccepted || accepted.Evidence != "codex thread.started" {
		t.Fatalf("accepted evidence outcome = %+v", accepted)
	}
	unproven := Evidence{}.Outcome(job.StatusInterrupted, job.Failure{Cause: job.CauseInterrupted})
	if unproven.PromptState != job.PromptUnknown || unproven.Evidence != "" {
		t.Fatalf("unproven evidence outcome = %+v", unproven)
	}
}

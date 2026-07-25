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
	ws := job.Workspace{Dir: "/tmp/x"}

	fresh := newClaude(Spec{Provider: "claude"}, time.Now())
	argv := fresh.Argv(ws)
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--session-id", fresh.sessionID}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("fresh argv = %v, want %v", argv, want)
	}
	if fresh.PreflightSessionID() == "" {
		t.Fatal("fresh claude must mint a session id before spawn")
	}

	budget := 0.5
	full := newClaude(Spec{
		Provider: "claude", Model: "opus", Effort: "xhigh",
		Resume: "abc", AllowWrite: true, MaxBudgetUSD: &budget,
	}, time.Now())
	argv = full.Argv(ws)
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

	fresh := newCodex(Spec{Provider: "codex"}, ws)
	argv := fresh.Argv(ws)
	want := []string{"exec", "--json", "-o", ws.LastMessagePath(), "-"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("fresh argv = %v, want %v", argv, want)
	}
	if fresh.PreflightSessionID() != "" {
		t.Fatal("fresh codex must not know a session id before spawn")
	}

	resumed := newCodex(Spec{Provider: "codex", Model: "gpt-5.3", Effort: "xhigh", Resume: "tid"}, ws)
	argv = resumed.Argv(ws)
	want = []string{
		"exec", "resume", "--json", "-m", "gpt-5.3",
		"-c", "model_reasoning_effort=xhigh", "-o", ws.LastMessagePath(), "tid", "-",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("resume argv = %v, want %v", argv, want)
	}
}

func TestClaudeStreamParsing(t *testing.T) {
	c := newClaude(Spec{Provider: "claude", TimeoutMin: 30}, time.Now())

	events := c.Feed(`{"type":"system","subtype":"init","session_id":"s1"}`)
	if len(events) != 2 || events[1].Kind != KindNote || events[1].State != "provider-initialized" {
		t.Fatalf("init events = %+v", events)
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
	c := newClaude(Spec{Provider: "claude", TimeoutMin: 30}, time.Now())
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"real partial work"}]}}`)
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"boom"}]}}`)
	c.Feed(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","errors":["boom"]}`)

	out := c.Conclude(ExitInfo{Code: job.Ptr(7)})
	if out.Status != job.StatusFailed {
		t.Fatalf("status = %s", out.Status)
	}
	if !strings.Contains(out.ErrorText, "boom") {
		t.Fatalf("error = %q", out.ErrorText)
	}
	if out.Partial == nil || *out.Partial != "real partial work" {
		t.Fatalf("partial must keep real work and drop the error echo, got %v", out.Partial)
	}
}

func TestClaudeBudgetStop(t *testing.T) {
	budget := 0.25
	c := newClaude(Spec{Provider: "claude", TimeoutMin: 30, MaxBudgetUSD: &budget}, time.Now())
	c.Feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial"}]}}`)
	c.Feed(`{"type":"result","subtype":"error_max_budget_usd","session_id":"s1","usage":{"input_tokens":1}}`)

	out := c.Conclude(ExitInfo{Code: job.Ptr(1)})
	if out.Status != job.StatusFailed || !strings.Contains(out.ErrorText, "--max-budget-usd 0.25 cap") {
		t.Fatalf("budget outcome = %+v", out)
	}
	if !strings.Contains(out.NextAction, "--resume s1 --timeout-min 30") {
		t.Fatalf("budget next action must resume: %q", out.NextAction)
	}
}

func TestClaudeArrayAndNoiseTolerance(t *testing.T) {
	c := newClaude(Spec{Provider: "claude"}, time.Now())
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
	c := newCodex(Spec{Provider: "codex", TimeoutMin: 180}, ws)

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
	if got := c.Takeover(); got != "codex resume tid-1" {
		t.Fatalf("takeover = %q", got)
	}
}

func TestCodexTerminalEnvelopeWinsOverKill(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Spec{Provider: "codex", TimeoutMin: 1}, ws)
	c.Feed(`{"type":"thread.started","thread_id":"tid-2"}`)
	c.Feed(`{"type":"item.completed","item":{"type":"agent_message","text":"finished work"}}`)
	c.Feed(`{"type":"turn.completed","usage":{"input_tokens":1}}`)

	// Killed during residual cleanup: no exit code, but the envelope landed.
	out := c.Conclude(ExitInfo{Signal: job.Ptr("SIGKILL"), Terminated: true, TerminalType: "codex turn.completed"})
	if out.Status != job.StatusOK || out.Text != "finished work" {
		t.Fatalf("terminal envelope must win over the kill, got %+v", out)
	}
}

func TestCodexFailureAndRecovery(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Spec{Provider: "codex", TimeoutMin: 30}, ws)
	c.Feed(`{"type":"thread.started","thread_id":"tid-3"}`)
	c.Feed(`{"type":"turn.failed","error":{"message":"model exploded"}}`)

	out := c.Conclude(ExitInfo{Code: job.Ptr(1)})
	if out.Status != job.StatusFailed || !strings.Contains(out.ErrorText, "model exploded") {
		t.Fatalf("outcome = %+v", out)
	}
	if out.PromptState != job.PromptAccepted {
		t.Fatalf("thread.started must mean accepted, got %s", out.PromptState)
	}

	ev, _ := c.Recovery()
	if !ev.Accepted || ev.Label != "codex thread.started" {
		t.Fatalf("recovery = %+v", ev)
	}
}

func TestCodexNoResultUsesStderrTail(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	c := newCodex(Spec{Provider: "codex"}, ws)
	out := c.Conclude(ExitInfo{Code: job.Ptr(23), StderrTail: "a\n\nb\nc\nd\n"})
	if out.Status != job.StatusInfra {
		t.Fatalf("status = %s", out.Status)
	}
	if !strings.Contains(out.ErrorText, "b | c | d") {
		t.Fatalf("stderr detail must keep the last three non-empty lines: %q", out.ErrorText)
	}
	if out.PromptState != job.PromptUnknown {
		t.Fatalf("prompt state = %s", out.PromptState)
	}
}

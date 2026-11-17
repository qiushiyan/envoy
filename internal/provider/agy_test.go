package provider

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
)

// feedAgyFixture feeds a stream agy 1.5.10 wrote (testdata/agy-1.5.10-*.jsonl)
// line by line and returns every event the driver emitted.
func feedAgyFixture(t *testing.T, a *agy, name string) []Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "agy-1.5.10-"+name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for line := range strings.SplitSeq(string(data), "\n") {
		events = append(events, a.Feed(line)...)
	}
	return events
}

func hasEvent(events []Event, match func(Event) bool) bool { return slices.ContainsFunc(events, match) }

// agy runs print mode with the prompt on stdin and the stream as JSON lines,
// prompts sent as written and permission prompts bypassed on every launch,
// each setting only when it was set, the conversation it resumes by id, and
// the standing instructions as a workspace folder on every launch.
func TestAgyArgv(t *testing.T) {
	ws := job.Workspace{Dir: "/tmp/x"}
	fixed := []string{"--disable-slash-commands", "--dangerously-skip-permissions"}

	fresh := newAgy(Options{}, ws, time.Now())
	if fresh.PreflightSessionID() != "" {
		t.Fatal("fresh agy must not know a conversation before spawn")
	}
	expectArgv(t, "fresh", fresh.Argv(), map[string]string{"--output-format": "stream-json"}, fixed,
		[]string{"-p", "--print", "--model", "--effort", "--conversation", "--add-dir"})

	resumed := newAgy(Options{Model: "gemini-4-carbon", Effort: "high", Resume: "conv-1", Instructions: "stay read-only\n"}, ws, time.Now())
	if got := resumed.PreflightSessionID(); got != "conv-1" {
		t.Fatalf("resume session id = %q, want conv-1", got)
	}
	expectArgv(t, "resumed", resumed.Argv(),
		map[string]string{"--output-format": "stream-json", "--model": "gemini-4-carbon", "--effort": "high",
			"--conversation": "conv-1", "--add-dir": filepath.Join(ws.Dir, "agy-rules")},
		fixed, []string{"-p", "--print"})
}

// The rules folder holds one AGENTS.md that is the job's own archive, so
// what agy read is what was archived; a conversation without instructions
// gets no folder at all.
func TestAgyInstructionsAreTheArchive(t *testing.T) {
	ws := job.Workspace{Dir: t.TempDir()}
	if err := ws.Prepare([]byte("prompt\n"), "stay read-only\n"); err != nil {
		t.Fatal(err)
	}
	a := newAgy(Options{Instructions: "stay read-only\n"}, ws, time.Now())
	for range 2 { // a second Prepare over an existing link is not an error
		if err := a.Prepare(); err != nil {
			t.Fatal(err)
		}
	}
	rule := filepath.Join(ws.Dir, "agy-rules", "AGENTS.md")
	if got, err := os.ReadFile(rule); err != nil || string(got) != "stay read-only\n" {
		t.Fatalf("AGENTS.md reads %q (%v), want the archived instructions", got, err)
	}
	if target, err := os.Readlink(rule); err != nil || target != filepath.Join("..", "instructions.md") {
		t.Fatalf("AGENTS.md links to %q (%v), want the job's archive", target, err)
	}

	bare := job.Workspace{Dir: t.TempDir()}
	if err := newAgy(Options{}, bare, time.Now()).Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bare.Dir, "agy-rules")); !os.IsNotExist(err) {
		t.Fatalf("a conversation without instructions must get no rules folder: %v", err)
	}
}

// A fresh turn: init names the conversation and, with --model, the base
// model; the user's input proves acceptance; the result is terminal and
// carries the answer; and the result's usage, which counts the whole
// conversation, is this turn's because this turn started it.
func TestAgyFreshTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAgy(Options{Model: "gemini-4-carbon", Effort: "high"}, job.Workspace{}, time.Now())
	events := feedAgyFixture(t, a, "fresh")

	const conv = "6d3d2852-741b-43b3-9e61-2a0aef074b9a"
	if !hasEvent(events, func(e Event) bool { return e.Kind == KindSessionStarted && e.SessionID == conv }) {
		t.Fatalf("init must start the session: %+v", events)
	}
	if !hasEvent(events, func(e Event) bool { return e.Kind == KindModelReported && e.Model == "gemini-4-carbon" }) {
		t.Fatalf("init's model must be reported: %+v", events)
	}
	if !hasEvent(events, func(e Event) bool { return e.Kind == KindAccepted && e.Evidence == "agy user_input" }) {
		t.Fatalf("the user's input must prove acceptance: %+v", events)
	}
	if !hasEvent(events, func(e Event) bool { return e.Kind == KindTerminal && e.Terminal == "agy success" }) {
		t.Fatalf("the result must be terminal: %+v", events)
	}
	if !hasEvent(events, func(e Event) bool { return e.Kind == KindActivity && e.Type == "step_update/tool" }) {
		t.Fatalf("a step's activity must name its type: %+v", events)
	}

	out := a.Conclude(ExitInfo{Code: new(0)})
	if out.Status != job.StatusOK || out.Text != "MAGENTA-42\n" || out.PromptState != job.PromptAccepted {
		t.Fatalf("outcome = %+v", out)
	}
	want := job.Tokens{Input: new(int64(10874)), CacheRead: new(int64(7139)), Output: new(int64(451)), ReasoningOutput: new(int64(357))}
	if out.Tokens == nil || *out.Tokens.Input != *want.Input || *out.Tokens.CacheRead != *want.CacheRead ||
		*out.Tokens.Output != *want.Output || *out.Tokens.ReasoningOutput != *want.ReasoningOutput {
		t.Fatalf("tokens = %+v, want %+v", out.Tokens, want)
	}
}

// A resumed turn's result counts every turn of the conversation, so the
// turn's own usage is its steps', and the conversation it continued is the
// one it asked for.
func TestAgyResumedTurnCountsItsOwnSteps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAgy(Options{Resume: "6d3d2852-741b-43b3-9e61-2a0aef074b9a"}, job.Workspace{}, time.Now())
	events := feedAgyFixture(t, a, "resumed")
	if hasEvent(events, func(e Event) bool { return e.Kind == KindSessionStarted }) {
		t.Fatalf("a resume must not start a session: %+v", events)
	}
	out := a.Conclude(ExitInfo{Code: new(0)})
	if out.Status != job.StatusOK || out.Text != "7731\n" || out.SessionID != "" {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Tokens == nil || *out.Tokens.Input != 2436 || *out.Tokens.Output != 50 || *out.Tokens.CacheRead != 7139 {
		t.Fatalf("tokens = %+v, want this turn's step (2436 in, 50 out, 7139 cached)", out.Tokens)
	}
}

// agy answers a --conversation it cannot find with a fresh conversation and
// exit 0. That answer had none of the history it was sent to continue: the
// turn fails as not continued, and the new conversation, which holds the
// turn, is the session a follow-up continues.
func TestAgyReplacedConversationIsNotAContinuation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAgy(Options{Resume: "11111111-2222-4333-8444-555555555555"}, job.Workspace{}, time.Now())
	events := feedAgyFixture(t, a, "unknown-conversation")
	if !hasEvent(events, func(e Event) bool {
		return e.Kind == KindNote && slices.Contains(e.Fields, job.KV{K: "requested", V: "11111111-2222-4333-8444-555555555555"})
	}) {
		t.Fatalf("the progress note must name the conversation asked for: %+v", events)
	}
	out := a.Conclude(ExitInfo{Code: new(0), StderrTail: `warning: conversation "11111111-2222-4333-8444-555555555555" not found` + "\n"})
	if out.Status != job.StatusFailed || out.Failure.Cause != job.CauseSessionNotContinued {
		t.Fatalf("outcome = %+v", out)
	}
	if out.SessionID != "50d7c75a-396c-476c-97ba-00fcfcc987a4" || out.PromptState != job.PromptAccepted {
		t.Fatalf("session = %q, prompt = %s; want the new conversation, accepted", out.SessionID, out.PromptState)
	}
	if out.Partial == nil || *out.Partial != "Hi! How can I help you today?\n" {
		t.Fatalf("the answer it gave there must survive as partial output: %v", out.Partial)
	}
	if !slices.Equal(out.Failure.StderrTail, []string{`warning: conversation "11111111-2222-4333-8444-555555555555" not found`}) {
		t.Fatalf("stderr tail = %q, want agy's own warning", out.Failure.StderrTail)
	}
}

// A stop makes agy end the turn with a result of its own, ERROR
// "interrupted". That echoes the stop and is not the provider's ending, so it
// is not terminal and the runner records the stop it sent; what survives is
// the acceptance and the usage the turn had spent.
func TestAgyInterruptionIsNotItsOwnEnding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAgy(Options{}, job.Workspace{}, time.Now())
	events := feedAgyFixture(t, a, "interrupted")
	if hasEvent(events, func(e Event) bool { return e.Kind == KindTerminal }) {
		t.Fatalf("an interrupted result must not be terminal: %+v", events)
	}
	ev := a.Recovery()
	if !ev.Accepted || ev.Label != "agy user_input" {
		t.Fatalf("recovery = %+v, want acceptance from the user's input", ev)
	}
	if ev.Tokens == nil || *ev.Tokens.Input != 7746 || *ev.Tokens.Output != 590 {
		t.Fatalf("tokens = %+v, want the fresh conversation's usage", ev.Tokens)
	}
	// Ended by a signal envoy did not send, the result is the provider's word.
	out := a.Conclude(ExitInfo{Code: new(1)})
	if out.Status != job.StatusFailed || out.Failure.Cause != job.CauseProviderVerdict ||
		job.Deref(out.Failure.Message) != "interrupted" || job.Deref(out.Failure.Code) != "interrupted" {
		t.Fatalf("outcome = %+v / %+v", out, out.Failure)
	}
}

// An unknown model, or an effort the model lacks, is refused before agy
// creates a conversation: the prompt never ran, and agy's own words say why.
func TestAgyRefusalBeforeAConversationNeverStarted(t *testing.T) {
	a := newAgy(Options{Model: "gemini-4-carbon", Effort: "xhigh"}, job.Workspace{}, time.Now())
	events := feedAgyFixture(t, a, "model-refused")
	if !hasEvent(events, func(e Event) bool { return e.Kind == KindTerminal && e.Terminal == "agy error" }) {
		t.Fatalf("the refusal is agy's own ending: %+v", events)
	}
	out := a.Conclude(ExitInfo{Code: new(1)})
	if out.Status != job.StatusFailed || out.PromptState != job.PromptNotStarted || out.Failure.Cause != job.CauseProviderVerdict {
		t.Fatalf("outcome = %+v", out)
	}
	if !strings.Contains(job.Deref(out.Failure.Message), `gemini-4-carbon has no "xhigh" effort`) || out.Tokens != nil {
		t.Fatalf("failure = %+v, tokens = %+v", out.Failure, out.Tokens)
	}
}

// An ERROR result after the turn began is agy's verdict, with what it had
// written; an exit with no result at all is the stream's absence, with
// stderr's last words.
func TestAgyVerdictAndMissingResult(t *testing.T) {
	a := newAgy(Options{}, job.Workspace{}, time.Now())
	for _, line := range []string{
		`{"event":"init","conversation_id":"c1","init":{}}`,
		`{"event":"step_update","step_update":{"conversation_id":"c1","step_index":0,"state":"DONE","step_type":"user_input"}}`,
		`{"event":"step_update","step_update":{"conversation_id":"c1","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"half "}}`,
		`{"event":"step_update","step_update":{"conversation_id":"c1","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"an answer"}}`,
		`not json`,
		`{"event":"result","result":{"conversation_id":"c1","status":"ERROR","response":"","error":"model overloaded","finished_reason":"error"}}`,
	} {
		a.Feed(line)
	}
	out := a.Conclude(ExitInfo{Code: new(3)})
	if out.Status != job.StatusFailed || job.Deref(out.Failure.Message) != "model overloaded" || job.Deref(out.Failure.Code) != "error" {
		t.Fatalf("outcome = %+v / %+v", out, out.Failure)
	}
	if out.Partial == nil || *out.Partial != "half an answer" || out.PromptState != job.PromptAccepted {
		t.Fatalf("partial = %v, prompt = %s", out.Partial, out.PromptState)
	}

	silent := newAgy(Options{}, job.Workspace{}, time.Now())
	silent.Feed(`{"event":"init","conversation_id":"c2","init":{}}`)
	out = silent.Conclude(ExitInfo{Code: new(3), StderrTail: "AGY_ERROR: {\"status\":\"UNAVAILABLE\"}\n"})
	if out.Status != job.StatusInfra || out.Failure.Cause != job.CauseExitedWithoutEnvelope ||
		!slices.Equal(out.Failure.StderrTail, []string{`AGY_ERROR: {"status":"UNAVAILABLE"}`}) {
		t.Fatalf("outcome = %+v / %+v", out, out.Failure)
	}
}

// The model agy's transcript says answered — the full id, effort included —
// is reported when the result arrives; a step from before this turn is not
// this turn's.
func TestAgyReportsTheModelItsTranscriptNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	started := time.Date(2026, 11, 17, 21, 38, 9, 500_000_000, time.UTC)
	logs := filepath.Join(home, ".gemini", "antigravity-cli", "brain", "c1", ".system_generated", "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"step_index": 1, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-17T21:30:00Z", "model": "gemini-3.8-flash-low"}`,
		`{"step_index": 4, "source": "USER_EXPLICIT", "type": "USER_INPUT", "status": "DONE", "created_at": "2026-11-17T21:38:09Z", "content": "hi"}`,
		`{"step_index": 5, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-17T21:38:09Z", "model": "gemini-4-carbon-high"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(logs, "transcript.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newAgy(Options{Resume: "c1"}, job.Workspace{}, started)
	a.Feed(`{"event":"init","conversation_id":"c1","init":{}}`)
	events := a.Feed(`{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"ok"}}`)
	if !hasEvent(events, func(e Event) bool { return e.Kind == KindModelReported && e.Model == "gemini-4-carbon-high" }) {
		t.Fatalf("result events = %+v, want the transcript's model", events)
	}

	elsewhere := newAgy(Options{Resume: "c1"}, job.Workspace{}, started.Add(time.Hour))
	events = elsewhere.Feed(`{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"ok"}}`)
	if hasEvent(events, func(e Event) bool { return e.Kind == KindModelReported }) {
		t.Fatalf("a transcript with no step of this turn reports no model: %+v", events)
	}
}

package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
)

// agy drives Google's Antigravity CLI headless: `agy --output-format
// stream-json` with the prompt on stdin. Without -p, print mode takes a piped
// stdin as the prompt; -p would carry it in argv, and `-p=-` sends a literal
// "-". The stream is one `init` (the conversation id), `step_update`s (the
// user's input, tool calls, the reply's text deltas, each model step's usage)
// and one `result` (status, answer, error, finished_reason).
//
// Two flags ride every launch, whatever the write intent:
//   - --disable-slash-commands: print mode otherwise reads a prompt that
//     starts with "/" as a command or skill, and `/model` answers SUCCESS with
//     no turn at all. A prompt is sent as written.
//   - --dangerously-skip-permissions: headless agy soft-denies every tool
//     that asks for a confirmation, and a project's own permission rules can
//     ask; there is no one to answer, as with claude's bypass. Its mode and
//     sandbox stay the user's configuration.
type agy struct {
	opts      Options
	ws        job.Workspace
	startedAt time.Time
	sessionID string // the conversation; learned from init when this turn starts one
	initSeen  bool
	// replaced is the conversation agy started in place of the one this turn
	// resumed: agy answers an id it cannot find with a warning on stderr and
	// a fresh conversation. "" when it continued the one asked for.
	replaced string
	// evidence is how acceptance was first proven; "" while it is unproven.
	evidence string
	// result is the first result record, nil until one arrives.
	result  map[string]any
	replies []*agyReply
	// steps is this launch's per-step usage, summed; counted guards each step
	// against a repeated DONE record.
	steps   agyUsage
	counted map[int64]bool
}

type agyReply struct {
	step int64
	text strings.Builder
}

// agyUsage sums the usage records agy puts on its model steps.
type agyUsage struct {
	seen                                bool
	input, cacheRead, output, reasoning int64
}

func (u *agyUsage) add(usage map[string]any) {
	u.seen = true
	u.input += numOr(usage, "input_tokens", 0)
	u.cacheRead += numOr(usage, "cache_read_tokens", 0)
	u.output += numOr(usage, "output_tokens", 0)
	u.reasoning += numOr(usage, "thinking_tokens", 0)
}

// tokens maps agy's usage onto the record: input excludes cache reads, and
// output includes the thinking tokens it also reports on their own.
func (u *agyUsage) tokens() *job.Tokens {
	if !u.seen {
		return nil
	}
	return &job.Tokens{
		Input:           new(u.input),
		CacheRead:       new(u.cacheRead),
		Output:          new(u.output),
		ReasoningOutput: new(u.reasoning),
	}
}

// agyRulesDir is the folder every launch adds to agy's workspace so that agy
// loads the conversation's standing instructions as a user rule: it holds
// only AGENTS.md, a link to the job's own archive.
const agyRulesDir = "agy-rules"

func newAgy(opts Options, ws job.Workspace, startedAt time.Time) *agy {
	return &agy{opts: opts, ws: ws, startedAt: startedAt, sessionID: opts.Resume, counted: make(map[int64]bool)}
}

func (a *agy) PreflightSessionID() string { return a.sessionID }

func (a *agy) rulesDir() string { return filepath.Join(a.ws.Dir, agyRulesDir) }

// Prepare links the rules folder's AGENTS.md to the job's archive, so the
// file agy read is the archive of what was sent. agy has no flag that adds to
// its system prompt: a root AGENTS.md of an added workspace folder is loaded
// as a user rule beside its own prompt, where a custom agent (--agent) would
// replace that prompt.
func (a *agy) Prepare() error {
	if a.opts.Instructions == "" {
		return nil
	}
	if err := os.MkdirAll(a.rulesDir(), 0o755); err != nil {
		return err
	}
	link := filepath.Join(a.rulesDir(), "AGENTS.md")
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(filepath.Join("..", filepath.Base(a.ws.InstructionsPath())), link)
}

func (a *agy) Argv() []string {
	args := []string{"--output-format", "stream-json", "--disable-slash-commands", "--dangerously-skip-permissions"}
	if a.opts.Model != "" {
		args = append(args, "--model", a.opts.Model)
	}
	if a.opts.Effort != "" {
		args = append(args, "--effort", a.opts.Effort)
	}
	if a.opts.Resume != "" {
		args = append(args, "--conversation", a.opts.Resume)
	}
	// On every launch, a resume included, as for every provider: the rule is
	// read from the workspace a launch was given.
	if a.opts.Instructions != "" {
		args = append(args, "--add-dir", a.rulesDir())
	}
	return args
}

func (a *agy) ExtraEnv() []string { return nil }

func (a *agy) Feed(line string) []Event {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
		return nil // foreign output stays in raw.log
	}
	typ := str(event, "event")
	if typ == "" {
		typ = "unknown"
	}
	switch typ {
	case "init":
		return a.onInit(event)
	case "step_update":
		return a.onStep(event)
	case "result":
		return a.onResult(event)
	}
	return []Event{{Kind: KindActivity, Type: typ}}
}

func (a *agy) onInit(event map[string]any) []Event {
	events := []Event{{Kind: KindActivity, Type: "init"}}
	a.initSeen = true
	conversation := str(event, "conversation_id")
	fields := []job.KV{{K: "session", V: conversation}}
	if a.opts.Resume != "" && conversation != "" && conversation != a.opts.Resume {
		a.replaced = conversation
		fields = append(fields, job.KV{K: "requested", V: a.opts.Resume})
	}
	// init names the model only when --model was passed, and then only as the
	// base model; the transcript names what answered (see onResult).
	init, _ := event["init"].(map[string]any)
	if model := str(init, "model"); model != "" {
		fields = append(fields, job.KV{K: "model", V: model})
		events = append(events, Event{Kind: KindModelReported, Model: model})
	}
	events = append(events, Event{Kind: KindNote, State: "provider-initialized", Fields: fields})
	if a.opts.Resume == "" && conversation != "" {
		a.sessionID = conversation
		events = append(events, Event{Kind: KindSessionStarted, SessionID: conversation})
	}
	return events
}

func (a *agy) onStep(event map[string]any) []Event {
	step, _ := event["step_update"].(map[string]any)
	stepType := str(step, "step_type")
	events := []Event{{Kind: KindActivity, Type: "step_update/" + stepType}}
	index := numOr(step, "step_index", -1)
	done := str(step, "state") == "DONE"
	// The user's input is recorded in the conversation before the model is
	// called; a turn stopped after it holds the prompt (EVIDENCE.md,
	// 2026-11-17), so a resume continues from it.
	if stepType == "user_input" && done && a.evidence == "" {
		a.evidence = "agy user_input"
		events = append(events, Event{Kind: KindAccepted, Evidence: a.evidence})
	}
	if delta := str(step, "text_delta"); stepType == "agent_response" && delta != "" {
		if n := len(a.replies); n == 0 || a.replies[n-1].step != index {
			a.replies = append(a.replies, &agyReply{step: index})
		}
		a.replies[len(a.replies)-1].text.WriteString(delta)
	}
	if usage, ok := step["usage"].(map[string]any); ok && done && !a.counted[index] {
		a.counted[index] = true
		a.steps.add(usage)
	}
	return events
}

func (a *agy) onResult(event map[string]any) []Event {
	events := []Event{{Kind: KindActivity, Type: "result"}}
	result, _ := event["result"].(map[string]any)
	if result == nil || a.result != nil {
		return events
	}
	a.result = result
	// A result that only echoes a stop envoy sent is not the provider's own
	// ending: left unmarked, the runner records the stop it requested.
	if str(result, "finished_reason") != "interrupted" {
		events = append(events, Event{Kind: KindTerminal, Terminal: "agy " + strings.ToLower(str(result, "status"))})
	}
	if model := agyTranscriptModel(str(result, "conversation_id"), a.startedAt); model != "" {
		events = append(events, Event{Kind: KindModelReported, Model: model})
	}
	return events
}

func (a *agy) Poll() []Event { return nil }

// tokens is this turn's usage. The result's usage counts the whole
// conversation — every earlier turn of a resumed one too — so it is this
// turn's alone only when this turn started the conversation; otherwise this
// launch's steps are summed.
func (a *agy) tokens() *job.Tokens {
	if a.result != nil && str(a.result, "conversation_id") != "" && (a.opts.Resume == "" || a.replaced != "") {
		if usage, ok := a.result["usage"].(map[string]any); ok {
			var u agyUsage
			u.add(usage)
			return u.tokens()
		}
	}
	return a.steps.tokens()
}

// replyText is the reply text this launch streamed, one paragraph per model
// step that wrote any.
func (a *agy) replyText() string {
	var parts []string
	for _, r := range a.replies {
		if text := strings.TrimSpace(r.text.String()); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (a *agy) response() *string {
	if a.result == nil {
		return nil
	}
	return new(str(a.result, "response"))
}

func (a *agy) Recovery() Evidence {
	return Evidence{
		Accepted: a.evidence != "",
		Label:    a.evidence,
		Partial:  firstText(a.response(), new(a.replyText())),
		Tokens:   a.tokens(),
	}
}

func (a *agy) Conclude(exit ExitInfo) Outcome {
	ev := a.Recovery()
	if a.result == nil {
		return ev.Outcome(job.StatusInfra,
			job.Failure{Cause: job.CauseExitedWithoutEnvelope, StderrTail: stderrLines(exit.StderrTail)})
	}
	verdict := job.Failure{Cause: job.CauseProviderVerdict,
		Message: job.PtrIfNonEmpty(str(a.result, "error")), Code: job.PtrIfNonEmpty(str(a.result, "finished_reason"))}

	if !a.initSeen && str(a.result, "conversation_id") == "" {
		// agy refused before it created a conversation — a model it does not
		// know, an effort that model lacks — so no prompt ran.
		out := ev.Outcome(job.StatusFailed, verdict)
		out.PromptState, out.Evidence = job.PromptNotStarted, "agy refused before starting a conversation"
		return out
	}
	if a.replaced != "" {
		// The turn ran, in a conversation without the history it was sent
		// to continue: whatever it answered is not the continuation asked
		// for. The new conversation holds this turn, so it is the session a
		// follow-up continues.
		out := ev.Outcome(job.StatusFailed,
			job.Failure{Cause: job.CauseSessionNotContinued, StderrTail: stderrLines(exit.StderrTail)})
		out.SessionID = a.replaced
		return out
	}
	if str(a.result, "status") == "SUCCESS" {
		evidence := ev.Label
		if evidence == "" {
			evidence = "agy success"
		}
		return Outcome{
			Status:      job.StatusOK,
			Text:        str(a.result, "response"),
			Tokens:      ev.Tokens,
			PromptState: job.PromptAccepted,
			Evidence:    evidence,
		}
	}
	return ev.Outcome(job.StatusFailed, verdict)
}

// ObservesConnectionErrors: agy retries rate limits and dropped connections
// inside the CLI and reports only a terminal failure.
func (a *agy) ObservesConnectionErrors() bool { return false }

func (a *agy) Usage() *job.Usage { return nil }

// agyTranscriptModel is the model agy's own transcript says answered this
// turn — the full id, effort included (gemini-4-carbon-high) — "" when it
// cannot be read. The stream's init names only the base model, and only when
// --model was passed. Best-effort in envoy's own HOME, like claude's
// transcript: a launcher may give the provider another, and recovery
// telemetry never changes an outcome, so every error collapses to "".
func agyTranscriptModel(conversation string, since time.Time) string {
	if conversation == "" || !safeSessionID.MatchString(conversation) {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".gemini", "antigravity-cli", "brain", conversation,
		".system_generated", "logs", "transcript.jsonl")
	text := tailText(path, 1<<20)
	// created_at has whole seconds; a step of this turn can carry the second
	// the turn started in.
	since = since.Truncate(time.Second)
	model := ""
	for raw := range strings.SplitSeq(text, "\n") {
		if !strings.Contains(raw, `"PLANNER_RESPONSE"`) {
			continue
		}
		var step map[string]any
		if json.Unmarshal([]byte(raw), &step) != nil || str(step, "type") != "PLANNER_RESPONSE" {
			continue
		}
		at, err := time.Parse(time.RFC3339, str(step, "created_at"))
		if err != nil || at.Before(since) {
			continue
		}
		if m := str(step, "model"); m != "" {
			model = m
		}
	}
	return model
}

// tailText is the last maxBytes of a file, starting at a whole line; "" when
// it cannot be read. A transcript of a long conversation grows large, and
// only this turn's end of it matters.
func tailText(path string, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := max(info.Size()-maxBytes, 0)
	buf := make([]byte, info.Size()-start)
	if n, err := f.ReadAt(buf, start); err != nil && n != len(buf) {
		return ""
	}
	text := string(buf)
	if start > 0 {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			return ""
		}
		text = text[i+1:]
	}
	return text
}

package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
)

// claude drives `claude -p --output-format stream-json --verbose`.
// stream-json emits completed message/tool events as they happen, while the
// final `result` event retains the same status/usage envelope as json mode.
// Partial token deltas stay off: event-boundary progress is useful without
// multiplying log size with one record per generated chunk.
type claude struct {
	opts      Options
	startedAt time.Time
	sessionID string
	accepted  bool
	evidence  string
	messages  []map[string]any
	usage     claudeUsage
}

func newClaude(opts Options, startedAt time.Time) *claude {
	sessionID := opts.Resume
	if sessionID == "" {
		// Claude accepts a caller-minted session id, so the lock and the
		// coordinates exist before spawn.
		sessionID = job.UUID4()
	}
	return &claude{opts: opts, startedAt: startedAt, sessionID: sessionID,
		usage: claudeUsage{value: job.NewUsage(), seen: make(map[string]struct{})}}
}

func (c *claude) PreflightSessionID() string { return c.sessionID }

func (c *claude) Argv() []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if c.opts.Model != "" {
		args = append(args, "--model", c.opts.Model)
	}
	if c.opts.Effort != "" {
		args = append(args, "--effort", c.opts.Effort)
	}
	if c.opts.Resume != "" {
		args = append(args, "--resume", c.opts.Resume)
	} else {
		args = append(args, "--session-id", c.sessionID)
	}
	// Write intent, not a sandbox: bypassPermissions lets the delegate edit and
	// run unattended. Without it the turn stays effectively read-only
	// (unpermitted tools fail; headless never prompts).
	if c.opts.AllowWrite {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	if c.opts.MaxBudgetUSD != nil {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%g", *c.opts.MaxBudgetUSD))
	}
	return args
}

func (c *claude) ExtraEnv() []string {
	// Claude's native stall watchdog; a Claude API knob, never set on codex.
	return []string{"API_FORCE_IDLE_TIMEOUT=1"}
}

func (c *claude) Feed(line string) []Event {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		// Keep foreign/malformed output in raw.log; a later valid result can
		// still complete the turn, and the raw protocol stays diagnosable.
		return nil
	}
	return c.consume(parsed)
}

func (c *claude) consume(parsed any) []Event {
	if arr, ok := parsed.([]any); ok {
		var events []Event
		for _, item := range arr {
			events = append(events, c.consume(item)...)
		}
		return events
	}
	event, ok := parsed.(map[string]any)
	if !ok {
		return nil
	}
	c.messages = append(c.messages, event)
	typ := str(event, "type")
	if typ == "" {
		typ = "unknown"
	}
	events := []Event{{Kind: KindActivity, Type: typ}}
	if id := str(event, "session_id"); id != "" && c.sessionID == "" {
		c.sessionID = id
	}
	subtype := str(event, "subtype")
	if typ == "system" && subtype == "init" {
		session := str(event, "session_id")
		if session == "" {
			session = c.sessionID
		}
		if session == "" {
			session = "pending"
		}
		fields := []job.KV{{K: "session", V: session}}
		if model := str(event, "model"); model != "" {
			fields = append(fields, job.KV{K: "model", V: model})
			events = append(events, Event{Kind: KindModelReported, Model: model})
		}
		events = append(events, Event{Kind: KindNote, State: "provider-initialized", Fields: fields})
	} else if typ == "system" && subtype == "api_retry" {
		events = append(events, Event{Kind: KindNote, State: "provider-retry", Fields: []job.KV{
			{K: "attempt", V: event["attempt"]},
			{K: "max_retries", V: event["max_retries"]},
			{K: "retry_delay_ms", V: event["retry_delay_ms"]},
		}})
	}
	if claudeEventProvesAcceptance(typ, subtype) {
		label := "claude " + typ
		if subtype != "" {
			label += "/" + subtype
		}
		c.accepted = true
		if c.evidence == "" {
			c.evidence = label
		}
		events = append(events, Event{Kind: KindAccepted, Evidence: label})
	}
	if typ == "result" {
		label := subtype
		if label == "" {
			label = "result"
		}
		events = append(events, Event{Kind: KindTerminal, Terminal: "claude " + label})
	}
	if c.usage.observe(event) {
		events = append(events, Event{Kind: KindUsage, Usage: c.Usage()})
	}
	return events
}

// system/init proves only that the process launched. User/assistant message
// events and partial stream events prove that this turn reached model work.
func claudeEventProvesAcceptance(typ, subtype string) bool {
	if typ == "assistant" || typ == "user" || typ == "stream_event" {
		return true
	}
	return typ == "result" && (subtype == "success" || subtype == "error_max_budget_usd")
}

// Poll is the heartbeat hook: while acceptance is unproven, look for
// same-turn evidence in Claude's persisted session transcript.
func (c *claude) Poll() []Event {
	if c.accepted {
		return nil
	}
	if t := claudeTranscript(c.sessionID, c.startedAt); t != nil && t.accepted {
		c.accepted = true
		c.evidence = "claude session transcript"
		return []Event{{Kind: KindAccepted, Evidence: "claude session transcript"}}
	}
	return nil
}

func (c *claude) Recovery() (Evidence, []Event) {
	parsed := parseClaudeMessages(c.messages)
	exclude := ""
	if parsed.kind == "failed" {
		exclude = parsed.errorText
	}
	streamPartial := assistantText(c.messages, exclude)
	transcript := claudeTranscript(c.sessionID, c.startedAt)

	var events []Event
	if !c.accepted && transcript != nil && transcript.accepted {
		c.accepted = true
		c.evidence = "claude session transcript"
		events = append(events, Event{Kind: KindAccepted, Evidence: "claude session transcript"})
	}

	var envelopePartial *string
	switch parsed.kind {
	case "ok":
		envelopePartial = job.Ptr(parsed.text)
	case "unparseable":
		envelopePartial = nil
	default:
		envelopePartial = job.Ptr(parsed.partial)
	}
	var transcriptPartial *string
	if transcript != nil {
		transcriptPartial = job.Ptr(transcript.partial)
	}
	ev := Evidence{
		Accepted: c.accepted,
		Label:    c.evidence,
		Partial:  firstText(envelopePartial, job.Ptr(streamPartial), transcriptPartial),
	}
	if parsed.kind != "unparseable" {
		ev.Tokens = parsed.tokens
		ev.CostUSD = parsed.costUSD
	}
	return ev, events
}

func (c *claude) Conclude(exit ExitInfo) Outcome {
	parsed := parseClaudeMessages(c.messages)
	if parsed.kind == "unparseable" {
		observed, _ := c.Recovery()
		out := Outcome{
			Status: job.StatusInfra,
			ErrorText: prose.ProcessExited("Claude", exit.Command, exit.Code,
				"but returned no parseable result envelope", stderrDetail(exit.StderrTail)),
			Partial:             observed.Partial,
			PromptState:         job.PromptUnknown,
			PromptStateEvidence: labelPtr(observed.Label),
			HasEvidence:         true,
		}
		if observed.Accepted {
			out.PromptState = job.PromptAccepted
		}
		return out
	}

	// The result envelope's session id wins: a resumed claude conversation is
	// continued under a fresh id, and that id is the one takeover/resume needs.
	if parsed.sessionID != "" {
		c.sessionID = parsed.sessionID
	}

	switch parsed.kind {
	case "ok":
		return Outcome{
			Status:      job.StatusOK,
			Text:        parsed.text,
			Tokens:      parsed.tokens,
			CostUSD:     parsed.costUSD,
			PromptState: job.PromptAccepted,
			SessionID:   parsed.sessionID,
		}
	case "budget":
		budget := 0.0
		if c.opts.MaxBudgetUSD != nil {
			budget = *c.opts.MaxBudgetUSD
		}
		return Outcome{
			Status: job.StatusFailed,
			ErrorText: fmt.Sprintf(
				"Claude stopped at the --max-budget-usd %g cap after accepting the prompt.", budget),
			Remedy:      "Raise the budget cap before continuing.",
			Partial:     job.Ptr(parsed.partial),
			Tokens:      parsed.tokens,
			CostUSD:     parsed.costUSD,
			PromptState: job.PromptAccepted,
			SessionID:   parsed.sessionID,
		}
	default: // "failed"
		observed, _ := c.Recovery()
		out := Outcome{
			Status:              job.StatusFailed,
			ErrorText:           fmt.Sprintf("Claude reported a provider failure: %s", parsed.errorText),
			Remedy:              "Fix the cause it reported first.",
			Partial:             job.Ptr(parsed.partial),
			Tokens:              parsed.tokens,
			CostUSD:             parsed.costUSD,
			PromptState:         job.PromptUnknown,
			PromptStateEvidence: labelPtr(observed.Label),
			HasEvidence:         true,
			SessionID:           parsed.sessionID,
		}
		if observed.Accepted {
			out.PromptState = job.PromptAccepted
		}
		return out
	}
}

// ObservesConnectionErrors: claude's network failures arrive as the result
// envelope's own error text, and no classification of them has been built.
func (c *claude) ObservesConnectionErrors() bool { return false }

// ---------- result envelope parsing ----------

type claudeParse struct {
	kind      string // ok | failed | budget | unparseable
	sessionID string
	costUSD   *float64
	tokens    *job.Tokens
	text      string
	errorText string
	partial   string
}

func parseClaudeMessages(messages []map[string]any) claudeParse {
	var envelope map[string]any
	for _, m := range messages {
		if str(m, "type") == "result" {
			envelope = m
			break
		}
	}
	if envelope == nil {
		return claudeParse{kind: "unparseable"}
	}

	out := claudeParse{sessionID: str(envelope, "session_id")}
	if cost, ok := num(envelope, "total_cost_usd"); ok {
		out.costUSD = job.Ptr(cost)
	}
	if usage, ok := envelope["usage"].(map[string]any); ok {
		if input, ok := num(usage, "input_tokens"); ok {
			out.tokens = &job.Tokens{
				Input:         job.Ptr(int64(input)),
				CacheRead:     job.Ptr(numOr(usage, "cache_read_input_tokens", 0)),
				CacheCreation: job.Ptr(numOr(usage, "cache_creation_input_tokens", 0)),
				Output:        job.Ptr(numOr(usage, "output_tokens", 0)),
			}
		}
	}

	subtype := str(envelope, "subtype")
	if subtype == "error_max_budget_usd" {
		out.kind = "budget"
		out.partial = assistantText(messages, "")
		return out
	}
	isError, _ := envelope["is_error"].(bool)
	if isError || subtype != "success" {
		out.kind = "failed"
		if s := str(envelope, "result"); s != "" {
			out.errorText = s
		} else if errs, ok := envelope["errors"].([]any); ok && len(errs) > 0 {
			parts := make([]string, 0, len(errs))
			for _, e := range errs {
				parts = append(parts, fmt.Sprint(e))
			}
			out.errorText = strings.Join(parts, " | ")
		} else {
			out.errorText = fmt.Sprintf("turn failed (%s)", subtype)
		}
		// Recover real partial work; exclude the trailing assistant block that
		// just echoes the error itself.
		out.partial = assistantText(messages, out.errorText)
		return out
	}
	out.kind = "ok"
	out.text = str(envelope, "result")
	return out
}

func assistantText(messages []map[string]any, excludeText string) string {
	exclude := strings.TrimSpace(excludeText)
	var parts []string
	for _, m := range messages {
		if str(m, "type") != "assistant" {
			continue
		}
		message, ok := m["message"].(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		var blocks []string
		for _, b := range content {
			block, ok := b.(map[string]any)
			if !ok || str(block, "type") != "text" {
				continue
			}
			text := str(block, "text")
			if strings.TrimSpace(text) == "" || strings.TrimSpace(text) == exclude {
				continue
			}
			blocks = append(blocks, text)
		}
		if len(blocks) > 0 {
			parts = append(parts, strings.Join(blocks, ""))
		}
	}
	return strings.Join(parts, "\n\n")
}

// ---------- session transcript recovery ----------

type transcriptResult struct {
	accepted bool
	partial  string
}

var safeSessionID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// claudeTranscript reads same-turn evidence from Claude's persisted session
// transcript: the exact session filename, records from THIS turn only. Prior
// rows in a resumed conversation are not acceptance evidence. Best-effort:
// recovery telemetry must never change the turn outcome, so all errors
// collapse to nil.
func claudeTranscript(sessionID string, since time.Time) *transcriptResult {
	if sessionID == "" || !safeSessionID.MatchString(sessionID) {
		return nil
	}
	// Best-effort lookup in envoy's environment: a launcher may choose a
	// different child config dir that envoy cannot know. Shared projects
	// stores still work; never infer a config dir from the launcher command.
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		configDir = filepath.Join(home, ".claude")
	}
	projectsRoot := filepath.Join(configDir, "projects")
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		return nil
	}
	var transcript string
	var newest time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsRoot, entry.Name(), sessionID+".jsonl")
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if transcript == "" || info.ModTime().After(newest) {
			transcript = candidate
			newest = info.ModTime()
		}
	}
	if transcript == "" {
		return nil
	}

	// Positional tail read: transcripts of long conversations reach tens of
	// MB, and this runs on every heartbeat while acceptance is unproven —
	// never load the whole file.
	const maxBytes = 1 << 20
	f, err := os.Open(transcript)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	start := info.Size() - maxBytes
	truncated := start > 0
	if start < 0 {
		start = 0
	}
	buf := make([]byte, info.Size()-start)
	if len(buf) > 0 {
		if n, rerr := f.ReadAt(buf, start); rerr != nil && n != len(buf) {
			return nil
		}
	}
	text := string(buf)
	if truncated {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		} else {
			text = ""
		}
	}

	var records []map[string]any
	result := &transcriptResult{}
	for raw := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
			continue
		}
		var record map[string]any
		if json.Unmarshal([]byte(raw), &record) != nil {
			continue
		}
		typ := str(record, "type")
		if typ != "user" && typ != "assistant" && typ != "result" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, str(record, "timestamp"))
		if err != nil || ts.Before(since) {
			continue
		}
		result.accepted = true
		records = append(records, record)
	}
	result.partial = assistantText(records, "")
	return result
}

// ---------- loosely-typed JSON access ----------

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func num(m map[string]any, key string) (float64, bool) {
	f, ok := m[key].(float64)
	return f, ok
}

func numOr(m map[string]any, key string, fallback int64) int64 {
	if f, ok := num(m, key); ok {
		return int64(f)
	}
	return fallback
}

func labelPtr(label string) *string {
	if label == "" {
		return nil
	}
	return job.Ptr(label)
}

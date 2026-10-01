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
	// evidence is how acceptance was first proven; "" while it is unproven.
	evidence string
	// envelope is the first result record, nil until one arrives.
	envelope map[string]any
	// replies is each assistant message's text, kept as the stream arrives so
	// the end of a turn never rescans it. Everything else the stream carried —
	// tool output included — is in raw.log, not in memory.
	replies [][]string
	usage   claudeUsage
}

// transcriptEvidence labels acceptance read from the session transcript.
const transcriptEvidence = "claude session transcript"

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
	// Every turn, whatever its write intent. An inherited mode depends on the
	// project's settings, the model and the auto classifier; where it lands on
	// `default`, headless has no one to answer a prompt, so reads outside the
	// cwd are refused and a consult answers without the files its brief cited.
	// Read-only is the prompt's job, not the engine's.
	args = append(args, "--permission-mode", "bypassPermissions")
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
	typ := str(event, "type")
	if typ == "" {
		typ = "unknown"
	}
	events := []Event{{Kind: KindActivity, Type: typ}}
	subtype := str(event, "subtype")
	switch {
	case typ == "system" && subtype == "init":
		session := str(event, "session_id")
		if session == "" {
			session = c.sessionID
		}
		fields := []job.KV{{K: "session", V: session}}
		if model := str(event, "model"); model != "" {
			fields = append(fields, job.KV{K: "model", V: model})
			events = append(events, Event{Kind: KindModelReported, Model: model})
		}
		events = append(events, Event{Kind: KindNote, State: "provider-initialized", Fields: fields})
	case typ == "system" && subtype == "api_retry":
		events = append(events, Event{Kind: KindNote, State: "provider-retry", Fields: []job.KV{
			{K: "attempt", V: event["attempt"]},
			{K: "max_retries", V: event["max_retries"]},
			{K: "retry_delay_ms", V: event["retry_delay_ms"]},
		}})
	case typ == "assistant":
		if blocks := replyText(event); len(blocks) > 0 {
			c.replies = append(c.replies, blocks)
		}
	case typ == "result" && c.envelope == nil:
		c.envelope = event
	}
	if claudeEventProvesAcceptance(typ, subtype) {
		label := "claude " + typ
		if subtype != "" {
			label += "/" + subtype
		}
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
	if c.evidence != "" {
		return nil
	}
	if t := claudeTranscript(c.sessionID, c.startedAt); t != nil && t.accepted {
		c.evidence = transcriptEvidence
		return []Event{{Kind: KindAccepted, Evidence: transcriptEvidence}}
	}
	return nil
}

func (c *claude) Recovery() Evidence { return c.recovery(c.parse()) }

// recovery is what the stream and the session transcript prove, given the
// envelope as parsed: acceptance, and the best output that survived.
func (c *claude) recovery(parsed claudeParse) Evidence {
	exclude := ""
	if parsed.kind == envelopeFailed {
		exclude = parsed.errorText
	}
	transcript := claudeTranscript(c.sessionID, c.startedAt)

	ev := Evidence{Accepted: c.evidence != "", Label: c.evidence}
	if !ev.Accepted && transcript != nil && transcript.accepted {
		ev.Accepted, ev.Label = true, transcriptEvidence
	}
	var envelopePartial, transcriptPartial *string
	switch parsed.kind {
	case envelopeOK:
		envelopePartial = job.Ptr(parsed.text)
	case envelopeFailed, envelopeBudget:
		envelopePartial = job.Ptr(parsed.partial)
	}
	if transcript != nil {
		transcriptPartial = job.Ptr(transcript.partial)
	}
	ev.Partial = firstText(envelopePartial, job.Ptr(joinReplies(c.replies, exclude)), transcriptPartial)
	if parsed.kind != envelopeMissing {
		ev.Tokens = parsed.tokens
		ev.CostUSD = parsed.costUSD
	}
	return ev
}

func (c *claude) Conclude(exit ExitInfo) Outcome {
	parsed := c.parse()
	if parsed.kind == envelopeMissing {
		// No envelope means no tokens or cost: recovery reads them from it.
		return c.recovery(parsed).Outcome(job.StatusInfra, prose.ProcessExited("Claude", exit.Command, exit.Code,
			"but returned no parseable result envelope", stderrDetail(exit.StderrTail)))
	}

	// The result envelope's session id wins: a resumed claude conversation is
	// continued under a fresh id, and that id is the one takeover/resume needs.
	if parsed.sessionID != "" {
		c.sessionID = parsed.sessionID
	}

	switch parsed.kind {
	case envelopeOK:
		return Outcome{
			Status:      job.StatusOK,
			Text:        parsed.text,
			Tokens:      parsed.tokens,
			CostUSD:     parsed.costUSD,
			PromptState: job.PromptAccepted,
			Evidence:    c.evidence,
			SessionID:   parsed.sessionID,
		}
	case envelopeBudget:
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
			Evidence:    c.evidence,
			SessionID:   parsed.sessionID,
		}
	default: // envelopeFailed
		out := c.recovery(parsed).Outcome(job.StatusFailed, fmt.Sprintf("Claude reported a provider failure: %s", parsed.errorText))
		out.Remedy = "Fix the cause it reported first."
		out.SessionID = parsed.sessionID
		return out
	}
}

// ObservesConnectionErrors: claude's network failures arrive as the result
// envelope's own error text, and no classification of them has been built.
func (c *claude) ObservesConnectionErrors() bool { return false }

// ---------- result envelope parsing ----------

// envelopeKind is what the result envelope says about the turn.
type envelopeKind int

const (
	envelopeMissing envelopeKind = iota // no result record arrived
	envelopeOK
	envelopeFailed
	envelopeBudget // stopped at the --max-budget-usd cap
)

type claudeParse struct {
	kind      envelopeKind
	sessionID string
	costUSD   *float64
	tokens    *job.Tokens
	text      string
	errorText string
	partial   string
}

// parse reads the result envelope, with the assistant text a non-ok ending
// recovers from the stream.
func (c *claude) parse() claudeParse {
	envelope := c.envelope
	if envelope == nil {
		return claudeParse{kind: envelopeMissing}
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
		out.kind = envelopeBudget
		out.partial = joinReplies(c.replies, "")
		return out
	}
	isError, _ := envelope["is_error"].(bool)
	if isError || subtype != "success" {
		out.kind = envelopeFailed
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
		out.partial = joinReplies(c.replies, out.errorText)
		return out
	}
	out.kind = envelopeOK
	out.text = str(envelope, "result")
	return out
}

// replyText is one assistant record's non-blank text blocks, in order.
func replyText(event map[string]any) []string {
	message, _ := event["message"].(map[string]any)
	content, _ := message["content"].([]any)
	var blocks []string
	for _, b := range content {
		block, ok := b.(map[string]any)
		if !ok || str(block, "type") != "text" {
			continue
		}
		if text := str(block, "text"); strings.TrimSpace(text) != "" {
			blocks = append(blocks, text)
		}
	}
	return blocks
}

// joinReplies is the assistant's text across its messages, leaving out any
// block that only repeats exclude — the trailing echo of an error the
// envelope already reports.
func joinReplies(replies [][]string, exclude string) string {
	exclude = strings.TrimSpace(exclude)
	var parts []string
	for _, blocks := range replies {
		var kept []string
		for _, text := range blocks {
			if strings.TrimSpace(text) != exclude {
				kept = append(kept, text)
			}
		}
		if len(kept) > 0 {
			parts = append(parts, strings.Join(kept, ""))
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

	var replies [][]string
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
		if blocks := replyText(record); typ == "assistant" && len(blocks) > 0 {
			replies = append(replies, blocks)
		}
	}
	result.partial = joinReplies(replies, "")
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

package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/qiushiyan/envoy/internal/job"
)

// codex drives `codex exec --json` and streams its JSONL events. The
// last-message file is retained as a recovery surface.
//
// No sandbox/permission flag EVER: the provider's own configuration governs.
// A derived read-only sandbox breaks the session's own tooling. The one fixed
// flag, --skip-git-repo-check, is a launch precondition, not a permission.
type codex struct {
	opts          Options
	ws            job.Workspace
	sessionID     string // learned from thread.started on a fresh thread
	threadStarted bool
	completed     bool // turn.completed arrived
	finalText     *string
	failed        bool    // turn.failed arrived: the provider's own verdict
	failedText    *string // its message, when it gave one
	lastErrorText *string // the latest transient `error` event; detail, never a verdict
	tokens        *job.Tokens
}

// connectionErrorMarkers are the substrings codex-cli 0.144.1 puts in the
// message of a transient `error` event when its link to the API drops:
// reconnect attempts, stream drops, DNS failures, transport fallback. A match
// is recorded as an observation of the provider's stream and nothing more —
// the engine never concludes "offline" from it. When codex changes these
// strings the count degrades to zero with raw.log still authoritative.
var connectionErrorMarkers = []string{
	"Reconnecting...",
	"stream disconnected",
	"Connection failed",
	"error sending request",
	"failed to lookup address",
	"waiting for network",
	"Falling back from WebSockets",
}

func isConnectionError(message string) bool {
	for _, marker := range connectionErrorMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func newCodex(opts Options, ws job.Workspace) *codex {
	return &codex{opts: opts, ws: ws, sessionID: opts.Resume}
}

func (c *codex) PreflightSessionID() string { return c.sessionID }

// Argv: `resume` takes the session id then `-` (prompt via stdin) and has no
// --cd; cwd is set on the child process instead.
func (c *codex) Argv() []string {
	var args []string
	if c.opts.Resume != "" {
		args = []string{"exec", "resume", "--json"}
	} else {
		args = []string{"exec", "--json"}
	}
	// On every launch, a resume included, whatever the write intent: codex
	// exec refuses a cwd with no git root above it before the thread starts
	// — trust entries in its config do not count — and a caller's cwd is
	// wherever its shell stands, a scratchpad or a new project as often as a
	// checkout. Inside a repository the flag changes nothing.
	args = append(args, "--skip-git-repo-check")
	if c.opts.Model != "" {
		args = append(args, "-m", c.opts.Model)
	}
	if c.opts.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+c.opts.Effort)
	}
	// On every launch, a resume included: codex keeps the first launch's
	// developer message in the thread and rebuilds it from the current
	// launch's config after a compaction. The setting has no file form that
	// adds to codex's own prompt (model_instructions_file replaces it), so the
	// text rides inline, as a TOML string so codex cannot read it as another
	// type.
	if c.opts.Instructions != "" {
		args = append(args, "-c", "developer_instructions="+tomlString(c.opts.Instructions))
	}
	args = append(args, "-o", c.ws.LastMessagePath())
	if c.opts.Resume != "" {
		args = append(args, c.opts.Resume)
	}
	return append(args, "-")
}

func (c *codex) ExtraEnv() []string { return nil }

// tomlString renders s as a TOML basic string: quotes, backslashes and every
// control character escaped, everything else literal, so codex's -c parser
// reads back exactly s.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (c *codex) Feed(line string) []Event {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		return nil // JSONL can carry non-JSON noise; it's in raw.log if it matters
	}
	event, isObject := parsed.(map[string]any)
	typ := "unknown"
	if isObject {
		if t := str(event, "type"); t != "" {
			typ = t
		}
	}
	events := []Event{{Kind: KindActivity, Type: typ}}
	if !isObject {
		return events
	}

	switch typ {
	case "thread.started":
		threadID := str(event, "thread_id")
		if threadID == "" {
			return events
		}
		c.threadStarted = true
		if c.sessionID == "" {
			c.sessionID = threadID
			events = append(events, Event{Kind: KindSessionStarted, SessionID: threadID})
		}
		events = append(events, Event{Kind: KindAccepted, Evidence: "codex thread.started"})
	case "item.completed":
		if item, ok := event["item"].(map[string]any); ok && str(item, "type") == "agent_message" {
			c.finalText = job.Ptr(str(item, "text"))
		}
	case "turn.completed":
		if usage, ok := event["usage"].(map[string]any); ok {
			c.tokens = &job.Tokens{
				Input:           job.Ptr(numOr(usage, "input_tokens", 0)),
				CachedInput:     job.Ptr(numOr(usage, "cached_input_tokens", 0)),
				Output:          job.Ptr(numOr(usage, "output_tokens", 0)),
				ReasoningOutput: job.Ptr(numOr(usage, "reasoning_output_tokens", 0)),
			}
		}
		c.completed = true
		events = append(events, Event{Kind: KindTerminal, Terminal: "codex turn.completed"})
	case "turn.failed":
		c.failed = true
		if errObj, ok := event["error"].(map[string]any); ok {
			c.failedText = job.PtrIfNonEmpty(str(errObj, "message"))
		}
		events = append(events, Event{Kind: KindTerminal, Terminal: "codex turn.failed"})
	case "error":
		// A bare `error` event is transient: codex emits one per reconnect
		// attempt and then carries on, and a turn that later completes is a
		// success. Only turn.failed is the provider's verdict.
		if m := str(event, "message"); m != "" {
			c.lastErrorText = job.Ptr(m)
			if isConnectionError(m) {
				events = append(events, Event{Kind: KindConnectionError})
			}
		}
	}
	return events
}

func (c *codex) Poll() []Event { return nil }

// recoveredText prefers the streamed agent message and falls back to the
// last-message file codex writes via -o.
func (c *codex) recoveredText() *string {
	if c.finalText != nil {
		return c.finalText
	}
	data, err := os.ReadFile(c.ws.LastMessagePath())
	if err != nil {
		return nil
	}
	return job.Ptr(string(data))
}

func (c *codex) Recovery() Evidence {
	partial := c.recoveredText()
	ev := Evidence{
		Accepted: c.threadStarted || partial != nil,
		Partial:  partial,
		Tokens:   c.tokens,
	}
	if c.threadStarted {
		ev.Label = "codex thread.started"
	} else if partial != nil {
		ev.Label = "codex recovered output"
	}
	return ev
}

func (c *codex) Conclude(exit ExitInfo) Outcome {
	ev := c.Recovery()
	if c.failed {
		return ev.Outcome(job.StatusFailed, job.Failure{Cause: job.CauseProviderVerdict, Message: c.failedText})
	}
	if ev.Partial == nil {
		return ev.Outcome(job.StatusInfra, job.Failure{Cause: job.CauseExitedWithoutResult,
			StderrTail: stderrLines(exit.StderrTail), LastErrorEvent: c.lastErrorText})
	}
	// The terminal envelope wins: if the hard cap fired only while the CLI or
	// a residual descendant was draining, an observed turn.completed is still
	// a success, not a timeout casualty.
	if (exit.Code != nil && *exit.Code == 0) || (exit.Terminated && c.completed) {
		return Outcome{
			Status:      job.StatusOK,
			Text:        *ev.Partial,
			Tokens:      c.tokens,
			PromptState: job.PromptAccepted,
			Evidence:    ev.Label,
		}
	}
	return ev.Outcome(job.StatusFailed, job.Failure{Cause: job.CauseExitedAfterResponse})
}

func (c *codex) ObservesConnectionErrors() bool { return true }

func (c *codex) Usage() *job.Usage { return nil }

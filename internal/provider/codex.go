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
// No sandbox/permission flag EVER: ~/.codex/config.toml governs. A derived
// read-only sandbox breaks the session's own tooling.
type codex struct {
	spec          Spec
	ws            job.Workspace
	sessionID     string // learned from thread.started on a fresh thread
	threadStarted bool
	finalText     *string
	errorText     *string
	tokens        *job.Tokens
}

func newCodex(spec Spec, ws job.Workspace) *codex {
	return &codex{spec: spec, ws: ws, sessionID: spec.Resume}
}

func (c *codex) Name() string               { return "codex" }
func (c *codex) Efforts() []string          { return efforts["codex"] }
func (c *codex) PreflightSessionID() string { return c.sessionID }

// Argv: `resume` takes the session id then `-` (prompt via stdin) and has no
// --cd; cwd is set on the child process instead.
func (c *codex) Argv(ws job.Workspace) []string {
	var args []string
	if c.spec.Resume != "" {
		args = []string{"exec", "resume", "--json"}
	} else {
		args = []string{"exec", "--json"}
	}
	if c.spec.Model != "" {
		args = append(args, "-m", c.spec.Model)
	}
	if c.spec.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+c.spec.Effort)
	}
	args = append(args, "-o", ws.LastMessagePath())
	if c.spec.Resume != "" {
		args = append(args, c.spec.Resume)
	}
	return append(args, "-")
}

func (c *codex) ExtraEnv() []string { return nil }

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
		events = append(events, Event{Kind: KindTerminal, Terminal: "codex turn.completed"})
	case "turn.failed":
		message := "turn failed"
		if errObj, ok := event["error"].(map[string]any); ok {
			if m := str(errObj, "message"); m != "" {
				message = m
			}
		}
		c.errorText = job.Ptr(message)
		events = append(events, Event{Kind: KindTerminal, Terminal: "codex turn.failed"})
	case "error":
		if m := str(event, "message"); m != "" {
			c.errorText = job.Ptr(m)
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

func (c *codex) Recovery() (Evidence, []Event) {
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
	return ev, nil
}

func (c *codex) Conclude(exit ExitInfo) Outcome {
	recovered := c.recoveredText()

	if c.errorText != nil {
		out := Outcome{
			Status:    job.StatusFailed,
			ErrorText: fmt.Sprintf("Codex reported a provider failure: %s", *c.errorText),
			Partial:   recovered,
			Tokens:    c.tokens,
		}
		if c.threadStarted || recovered != nil {
			out.PromptState = job.PromptAccepted
		} else {
			out.PromptState = job.PromptUnknown
		}
		if c.sessionID != "" {
			out.NextAction = fmt.Sprintf(
				"Inspect the partial output and working tree. Fix the reported cause, then continue with %s; do not resend completed work.",
				c.ResumeArgs())
		} else {
			out.NextAction = "Inspect progress.log, raw.log, stderr.log, and the working tree before retrying; the runtime cannot prove whether the provider began work."
		}
		return out
	}

	if recovered != nil {
		// The terminal envelope wins: if the hard cap fired only while the CLI
		// or a residual descendant was draining, an observed turn.completed is
		// still a success, not a timeout casualty.
		if (exit.Code != nil && *exit.Code == 0) || (exit.Terminated && exit.TerminalType == "codex turn.completed") {
			return Outcome{
				Status:      job.StatusOK,
				Text:        *recovered,
				Tokens:      c.tokens,
				PromptState: job.PromptAccepted,
			}
		}
		out := Outcome{
			Status:      job.StatusFailed,
			ErrorText:   fmt.Sprintf("Codex exited with code %s after producing a response.", codeStr(exit.Code)),
			Partial:     recovered,
			Tokens:      c.tokens,
			PromptState: job.PromptAccepted,
		}
		if c.sessionID != "" {
			out.NextAction = fmt.Sprintf(
				"Read the recovered output and inspect the working tree, then continue with %s if work remains.", c.ResumeArgs())
		} else {
			out.NextAction = "Read the recovered output and inspect the working tree before deciding whether another dispatch is needed."
		}
		return out
	}

	out := Outcome{
		Status: job.StatusInfra,
		ErrorText: fmt.Sprintf(
			"Codex exited with code %s but returned no usable result. Last provider detail: %s",
			codeStr(exit.Code), stderrDetail(exit.StderrTail)),
		Tokens: c.tokens,
	}
	if c.threadStarted {
		out.PromptState = job.PromptAccepted
	} else {
		out.PromptState = job.PromptUnknown
	}
	if c.threadStarted && c.sessionID != "" {
		out.NextAction = fmt.Sprintf(
			"The prompt was accepted. Inspect progress.log, raw.log, stderr.log, and the working tree, then continue with %s; do not redispatch the original prompt.",
			c.ResumeArgs())
	} else {
		out.NextAction = "Prompt acceptance is unconfirmed. Inspect progress.log, raw.log, stderr.log, and the working tree; retry unchanged only if they prove that work never began."
	}
	return out
}

func (c *codex) ResumeArgs() string { return resumeArgs(c.sessionID, c.spec.TimeoutMin) }

func (c *codex) Takeover() string {
	if c.sessionID == "" {
		return ""
	}
	return "codex resume " + c.sessionID
}

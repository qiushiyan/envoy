package provider

import (
	"math"
	"slices"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
)

type claudeUsage struct {
	value *job.Usage
	model string
	seen  map[string]struct{}
}

func (c *claude) Usage() *job.Usage {
	u := *c.usage.value
	u.Issues = slices.Clone(u.Issues)
	// Token pointers are immutable: each update replaces them.
	if u.TerminalTokens != nil {
		tokens := *u.TerminalTokens
		u.TerminalTokens = &tokens
	}
	return &u
}

func (c *claudeUsage) observe(event map[string]any) bool {
	u := c.value
	if u.Final || event["parent_tool_use_id"] != nil {
		return false
	}
	switch str(event, "type") {
	case "system":
		if str(event, "subtype") != "init" {
			return false
		}
		c.model = str(event, "model")
		if c.model != "" && u.Attribution == job.AttributionUnknown {
			u.Attribution = job.AttributionComplete
		}
	case "assistant":
		message, _ := event["message"].(map[string]any)
		id := str(message, "id")
		if id == "" {
			u.AddIssue(job.IssueMissingMessageID)
			break
		}
		if _, exists := c.seen[id]; exists {
			return false
		}
		c.seen[id] = struct{}{}
		model := str(message, "model")
		if model == "" || c.model == "" {
			u.Attribution = job.AttributionIncomplete
			u.AddIssue(job.IssueMissingModel)
			break
		}
		if model != c.model {
			u.Attribution = job.AttributionIncomplete
			u.UnexpectedModel = job.Ptr(model)
			u.AddIssue(job.IssueModelMismatch)
			break
		}
		u.Responses++
		usage, _ := message["usage"].(map[string]any)
		input, read, creation := usageInt(usage, "input_tokens"), usageInt(usage, "cache_read_input_tokens"), usageInt(usage, "cache_creation_input_tokens")
		if input == nil || read == nil || creation == nil {
			u.AddIssue(job.IssueMissingInputUsage)
			break
		}
		context := *input + *read + *creation
		u.LatestContextTokens = job.Ptr(context)
		if u.PeakContextTokens == nil || context > *u.PeakContextTokens {
			u.PeakContextTokens = job.Ptr(context)
		}
		u.SampledAt = job.Ptr(job.ISO(time.Now()))
		u.InputTokens = addUsage(u.InputTokens, *input)
		u.CacheReadInputTokens = addUsage(u.CacheReadInputTokens, *read)
		u.CacheCreationInputTokens = addUsage(u.CacheCreationInputTokens, *creation)
	case "result":
		c.settle(event)
	default:
		return false
	}
	u.RefreshState()
	return true
}

func (c *claudeUsage) settle(event map[string]any) {
	u := c.value
	u.Final = true
	models, _ := event["modelUsage"].(map[string]any)
	if c.model != "" {
		model, _ := models[c.model].(map[string]any)
		if window := usageInt(model, "contextWindow"); window != nil && *window > 0 {
			u.ContextWindowTokens = window
		}
	}
	if u.ContextWindowTokens == nil {
		u.AddIssue(job.IssueMissingWindow)
	}
	if u.LatestContextTokens == nil {
		u.AddIssue(job.IssueMissingContextSample)
	}
	usage, ok := event["usage"].(map[string]any)
	if !ok {
		u.AddIssue(job.IssueMissingTerminalUsage)
		return
	}
	t := &job.UsageTokens{
		Input: usageInt(usage, "input_tokens"), CacheRead: usageInt(usage, "cache_read_input_tokens"),
		CacheCreation: usageInt(usage, "cache_creation_input_tokens"), Output: usageInt(usage, "output_tokens"),
	}
	u.TerminalTokens = t
	// These envelopes can omit completed work, including the real output.
	if str(event, "subtype") == "error_max_budget_usd" {
		u.AddIssue(job.IssueTerminalUsageIncomplete)
		return
	}
	if t.Input == nil || t.CacheRead == nil || t.CacheCreation == nil || t.Output == nil {
		u.AddIssue(job.IssueMissingTerminalUsage)
		return
	}
	if str(event, "subtype") == "error_during_execution" && *t.Input == 0 && *t.CacheRead == 0 && *t.CacheCreation == 0 && *t.Output == 0 {
		u.AddIssue(job.IssueTerminalUsageIncomplete)
		return
	}
	if (u.InputTokens != nil && *u.InputTokens != *t.Input) ||
		(u.CacheReadInputTokens != nil && *u.CacheReadInputTokens != *t.CacheRead) ||
		(u.CacheCreationInputTokens != nil && *u.CacheCreationInputTokens != *t.CacheCreation) {
		u.AddIssue(job.IssueTerminalUsageMismatch)
		return
	}
	// A terminal total cannot repair attribution or prove missing responses.
	if u.Attribution != job.AttributionComplete || slices.Contains(u.Issues, job.IssueMissingInputUsage) || slices.Contains(u.Issues, job.IssueMissingMessageID) {
		u.AddIssue(job.IssueTerminalUsageIncomplete)
		return
	}
	u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens = t.Input, t.CacheRead, t.CacheCreation
	u.OutputTokens = t.Output
}

// Missing, fractional, negative, or inexact JSON integers are not measurements.
func usageInt(m map[string]any, key string) *int64 {
	f, ok := num(m, key)
	if !ok || f < 0 || f > 1<<53-1 || math.Trunc(f) != f {
		return nil
	}
	return job.Ptr(int64(f))
}

func addUsage(previous *int64, value int64) *int64 {
	if previous != nil {
		value += *previous
	}
	return job.Ptr(value)
}

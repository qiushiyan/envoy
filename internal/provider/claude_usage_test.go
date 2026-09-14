package provider

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
)

// Captured 2026-09-14 from a read-only three-response Claude Code 2.1.270
// turn. IDs were renamed; content/session/configuration were removed. Usage,
// duplicate ordering, and the helper-model breakdown are unchanged.
func usageCapture(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/claude-2.1.270-usage.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func feedUsage(t *testing.T, c *claude, record map[string]any) []Event {
	t.Helper()
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return c.Feed(string(b))
}

func TestClaudeUsageCapture(t *testing.T) {
	c := newClaude(Options{}, time.Now())
	if u := c.Usage(); u.State != "unmeasured" || u.LatestContextTokens != nil || u.OutputTokens != nil || u.ContextWindowTokens != nil || u.Final {
		t.Fatalf("initial measurement = %+v", u)
	}
	records := usageCapture(t)
	feedUsage(t, c, records[0])
	feedUsage(t, c, records[1])
	first := c.Usage()
	if first.State != "live" || first.Attribution != "complete" || first.Responses != 1 || *first.LatestContextTokens != 18889 || first.OutputTokens != nil || first.SampledAt == nil {
		t.Fatalf("first response = %+v", first)
	}
	for _, event := range feedUsage(t, c, records[2]) {
		if event.Kind == KindUsage {
			t.Fatal("duplicate must not produce a usage write")
		}
	}
	if !reflect.DeepEqual(first, c.Usage()) {
		t.Fatal("duplicate changed the sample, totals, count, or timestamp")
	}
	feedUsage(t, c, records[3])
	feedUsage(t, c, records[4])
	if *first.LatestContextTokens != 18889 || first.Responses != 1 {
		t.Fatal("later feeds mutated a previously emitted snapshot")
	}
	live := c.Usage()
	if live.Responses != 3 || live.OutputTokens != nil || live.ContextWindowTokens != nil || *live.LatestContextTokens != 19454 || *live.PeakContextTokens != 19454 {
		t.Fatalf("live = %+v", live)
	}
	feedUsage(t, c, records[5])
	u := c.Usage()
	if u.State != "settled" || !u.Final || len(u.Issues) != 0 || *u.InputTokens != 66 || *u.CacheReadInputTokens != 38178 || *u.CacheCreationInputTokens != 19422 || *u.OutputTokens != 338 || *u.ContextWindowTokens != 1000000 {
		t.Fatalf("settled = %+v", u)
	}
	if u.SampledAt == nil || *u.SampledAt != *live.SampledAt || u.Responses != 3 || *u.LatestContextTokens != 19454 {
		t.Fatal("terminal must not replace the last context sample with cumulative usage")
	}
	// First terminal wins, just like the lifecycle's terminal observation.
	feedUsage(t, c, records[1])
	feedUsage(t, c, records[5])
	if !reflect.DeepEqual(u, c.Usage()) {
		t.Fatal("post-terminal records changed settled usage")
	}
}

func TestClaudeUsageAttributionAndCompaction(t *testing.T) {
	c := newClaude(Options{}, time.Now())
	records := usageCapture(t)
	feedUsage(t, c, records[0])
	feedUsage(t, c, records[1])
	before := c.Usage()
	// Controlled variants of captured records exercise subagent attribution
	// and a lower post-compaction request without billing a long real turn.
	subagent := records[3]
	subagent["parent_tool_use_id"] = "tool_subagent"
	feedUsage(t, c, subagent)
	if !reflect.DeepEqual(before, c.Usage()) {
		t.Fatal("subagent record changed primary usage")
	}
	// A subagent must not reserve a primary response's dedup ID.
	subagent["parent_tool_use_id"] = nil
	feedUsage(t, c, subagent)
	valid := c.Usage()
	if valid.Responses != 2 {
		t.Fatal("subagent polluted primary dedup set")
	}
	mismatch := records[4]
	message := mismatch["message"].(map[string]any)
	message["model"] = "another-model"
	feedUsage(t, c, mismatch)
	u := c.Usage()
	if u.Attribution != "incomplete" || *u.UnexpectedModel != "another-model" || u.Responses != 2 || *u.LatestContextTokens != *valid.LatestContextTokens || *u.SampledAt != *valid.SampledAt {
		t.Fatalf("unattributed response = %+v", u)
	}
	message["id"], message["model"] = "after-compaction", records[0]["model"]
	message["usage"] = map[string]any{"input_tokens": 2, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 20, "output_tokens": 9999}
	feedUsage(t, c, mismatch)
	u = c.Usage()
	if *u.LatestContextTokens != 122 || *u.PeakContextTokens != 19323 || u.Responses != 3 || *u.InputTokens != 36 || *u.CacheReadInputTokens != 18987 || *u.CacheCreationInputTokens != 19311 || u.OutputTokens != nil {
		t.Fatalf("compaction sample = %+v", u)
	}
	if u.Attribution != "incomplete" || !slices.Contains(u.Issues, "model_mismatch") {
		t.Fatal("later valid sample erased attribution warning")
	}
	feedUsage(t, c, records[5])
	if c.Usage().State != "incomplete" || c.Usage().OutputTokens != nil {
		t.Fatal("terminal cannot repair ambiguous primary attribution")
	}
}

func TestClaudeUsageTerminalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		issue  string
		output bool
	}{
		{"missing usage", func(e map[string]any) { delete(e, "usage") }, "missing_terminal_usage", false},
		{"empty usage", func(e map[string]any) { e["usage"] = map[string]any{} }, "missing_terminal_usage", false},
		{"missing output", func(e map[string]any) { delete(e["usage"].(map[string]any), "output_tokens") }, "missing_terminal_usage", false},
		{"zeroed crash", func(e map[string]any) {
			e["subtype"] = "error_during_execution"
			e["usage"] = map[string]any{"input_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 0}
		}, "terminal_usage_incomplete", false},
		{"budget", func(e map[string]any) { e["subtype"] = "error_max_budget_usd" }, "terminal_usage_incomplete", false},
		{"contradiction", func(e map[string]any) { e["usage"].(map[string]any)["input_tokens"] = 1 }, "terminal_usage_mismatch", false},
		{"missing window", func(e map[string]any) { delete(e, "modelUsage") }, "missing_window", true},
		{"other model window", func(e map[string]any) { delete(e["modelUsage"].(map[string]any), "claude-fable-5-1") }, "missing_window", true},
		{"zero window", func(e map[string]any) {
			e["modelUsage"].(map[string]any)["claude-fable-5-1"].(map[string]any)["contextWindow"] = 0
		}, "missing_window", true},
		{"failed with complete usage", func(e map[string]any) { e["subtype"], e["is_error"] = "error_max_turns", true }, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClaude(Options{}, time.Now())
			records := usageCapture(t)
			for _, e := range records[:5] {
				feedUsage(t, c, e)
			}
			tc.change(records[5])
			feedUsage(t, c, records[5])
			u := c.Usage()
			if !u.Final || *u.InputTokens != 66 || *u.CacheReadInputTokens != 38178 || *u.CacheCreationInputTokens != 19422 || *u.LatestContextTokens != 19454 || *u.PeakContextTokens != 19454 {
				t.Fatalf("terminal erased observed evidence: %+v", u)
			}
			if (u.OutputTokens != nil) != tc.output {
				t.Fatalf("output known = %v, want %v", u.OutputTokens != nil, tc.output)
			}
			if tc.issue != "" && (u.State != "incomplete" || !slices.Contains(u.Issues, tc.issue)) {
				t.Fatalf("missing incompleteness: %+v", u)
			}
			if tc.issue == "" && u.State != "settled" {
				t.Fatalf("turn failure is independent of measurement completeness: %+v", u)
			}
			if tc.name == "contradiction" && *u.TerminalTokens.Input != 1 {
				t.Fatal("contradictory provider report must remain inspectable")
			}
		})
	}
}

func TestClaudeUsageMissingSampleEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, issue string
		change      func([]map[string]any)
	}{
		{"missing init model", "missing_model", func(r []map[string]any) { delete(r[0], "model") }},
		{"missing response model", "missing_model", func(r []map[string]any) { delete(r[1]["message"].(map[string]any), "model") }},
		{"missing ID", "missing_message_id", func(r []map[string]any) { delete(r[1]["message"].(map[string]any), "id") }},
		{"missing usage", "missing_input_usage", func(r []map[string]any) { delete(r[1]["message"].(map[string]any), "usage") }},
		{"missing cache", "missing_input_usage", func(r []map[string]any) {
			delete(r[1]["message"].(map[string]any)["usage"].(map[string]any), "cache_read_input_tokens")
		}},
		{"negative", "missing_input_usage", func(r []map[string]any) {
			r[1]["message"].(map[string]any)["usage"].(map[string]any)["input_tokens"] = -1
		}},
		{"fractional", "missing_input_usage", func(r []map[string]any) {
			r[1]["message"].(map[string]any)["usage"].(map[string]any)["input_tokens"] = 1.5
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := usageCapture(t)
			tc.change(r)
			c := newClaude(Options{}, time.Now())
			feedUsage(t, c, r[0])
			feedUsage(t, c, r[1])
			u := c.Usage()
			if u.LatestContextTokens != nil || u.InputTokens != nil || u.State != "unmeasured" || !slices.Contains(u.Issues, tc.issue) {
				t.Fatalf("invalid sample became a measurement: %+v", u)
			}
			feedUsage(t, c, r[5])
			if c.Usage().State != "incomplete" || c.Usage().OutputTokens != nil {
				t.Fatal("terminal silently repaired missing sample evidence")
			}
		})
	}
}

func TestClaudeUsageResumeStartsNewTurn(t *testing.T) {
	c := newClaude(Options{Resume: "previous-session"}, time.Now())
	if c.Usage().Responses != 0 || c.Usage().PeakContextTokens != nil {
		t.Fatal("continuation inherited earlier turn accounting")
	}
	r := usageCapture(t)
	feedUsage(t, c, r[0])
	feedUsage(t, c, r[4])
	if u := c.Usage(); u.Responses != 1 || *u.PeakContextTokens != 19454 || *u.InputTokens != 32 {
		t.Fatalf("resumed turn = %+v", u)
	}
	d, err := New("codex", Options{}, job.Workspace{Dir: t.TempDir()}, time.Now())
	if err != nil || d.Usage() != nil {
		t.Fatal("Codex must leave usage unavailable")
	}
}

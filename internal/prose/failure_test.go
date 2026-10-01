package prose

import (
	"testing"

	"github.com/qiushiyan/envoy/internal/job"
)

// A record holds a failure's cause and the provider's own words; the
// sentence is worded here, from the record, every time it is read. Each cause
// keeps the exact wording the engine published before records stopped
// carrying it — that wording is the caller contract.
func TestFailureIsWordedFromTheRecord(t *testing.T) {
	budget := 0.25
	bare := func(m job.Meta) *job.Meta {
		m.CommandPrefix = []string{m.Provider}
		return &m
	}
	launched := func(m job.Meta) *job.Meta {
		m.CommandPrefix = []string{"headroom", "launch", "--"}
		return &m
	}
	cases := []struct {
		name string
		meta *job.Meta
		want string
	}{
		{"spawn", bare(job.Meta{Provider: "codex", Failure: &job.Failure{Cause: job.CauseSpawnFailed, Message: job.Ptr("exec: not found")}}),
			"envoy could not start codex: exec: not found"},
		{"verdict", bare(job.Meta{Provider: "codex", Failure: &job.Failure{Cause: job.CauseProviderVerdict, Message: job.Ptr("model exploded")}}),
			"Codex reported a provider failure: model exploded"},
		{"verdict without words, with a code", bare(job.Meta{Provider: "claude", Failure: &job.Failure{Cause: job.CauseProviderVerdict, Code: job.Ptr("error_during_execution")}}),
			"Claude reported a provider failure: turn failed (error_during_execution)"},
		{"verdict without words", bare(job.Meta{Provider: "codex", Failure: &job.Failure{Cause: job.CauseProviderVerdict}}),
			"Codex reported a provider failure: turn failed"},
		{"budget", bare(job.Meta{Provider: "claude", MaxBudgetUSD: &budget, Failure: &job.Failure{Cause: job.CauseBudgetCap}}),
			"Claude stopped at the --max-budget-usd 0.25 cap after accepting the prompt."},
		{"exit after response", bare(job.Meta{Provider: "codex", ChildExitCode: job.Ptr(7), Failure: &job.Failure{Cause: job.CauseExitedAfterResponse}}),
			"Codex exited with code 7 after producing a response."},
		{"exit without result", bare(job.Meta{Provider: "codex", ChildExitCode: job.Ptr(1),
			Failure: &job.Failure{Cause: job.CauseExitedWithoutResult, StderrTail: []string{"b", "c"}, LastErrorEvent: job.Ptr("Reconnecting...")}}),
			`Codex exited with code 1 but returned no usable result. Last provider detail: last error event "Reconnecting..."; stderr: b | c`},
		{"launcher exit without envelope", launched(job.Meta{Provider: "claude", Failure: &job.Failure{Cause: job.CauseExitedWithoutEnvelope}}),
			`Command "headroom" exited with code null but returned no parseable result envelope. Last command detail: (no stderr detail)`},
		{"foreign signal", bare(job.Meta{Provider: "claude", ChildExitSignal: job.Ptr("SIGKILL"), Failure: &job.Failure{Cause: job.CauseForeignSignal}}),
			"claude was killed by signal SIGKILL, which envoy did not send."},
		{"interrupted", bare(job.Meta{Provider: "codex", InterruptionSignal: job.Ptr("SIGINT"), Failure: &job.Failure{Cause: job.CauseInterrupted}}),
			"envoy stopped codex after receiving SIGINT."},
		{"session conflict", bare(job.Meta{Provider: "codex", Failure: &job.Failure{Cause: job.CauseSessionConflict}}),
			"Codex reported a session id that another turn already holds, so this turn was stopped."},
		{"abandoned", bare(job.Meta{Provider: "codex", Failure: &job.Failure{Cause: job.CauseAbandoned}}),
			"This turn ended without publishing a result: the recorded runner and provider process group are no longer alive."},
		{"timeout", bare(job.Meta{Provider: "codex", TimeoutMin: 30, Failure: &job.Failure{Cause: job.CauseTimeout, Stream: &job.StreamSample{Events: 1, LastEvent: "thread.started", QuietMs: 90_000}}}),
			"The 30-minute wall-clock cap ended this codex turn. The cap counts healthy work too, so reaching it is not evidence the provider hung. When the cap arrived the stream had been quiet for 1m, after 1 event (last: thread.started)."},
		{"ok", bare(job.Meta{Provider: "codex"}), ""},
	}
	for _, c := range cases {
		if got := Failure(c.meta); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// result.md for a turn that did not deliver carries the status, the failure
// and any output recovered, in that order.
func TestFailedResultCarriesWhyAndWhatSurvived(t *testing.T) {
	m := &job.Meta{Status: job.StatusFailed, Provider: "codex", Failure: &job.Failure{Cause: job.CauseProviderVerdict, Message: job.Ptr("boom")}}
	want := "# Turn failed\n\nCodex reported a provider failure: boom\n\n## Partial output recovered before the failure\n\nhalf\n"
	if got := FailedResult(m, "half"); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := FailedResult(m, ""); got != "# Turn failed\n\nCodex reported a provider failure: boom\n" {
		t.Fatalf("no partial: %q", got)
	}
}

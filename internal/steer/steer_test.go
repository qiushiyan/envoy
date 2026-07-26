package steer

import (
	"strings"
	"testing"

	"github.com/qiushiyan/envoy/internal/job"
)

// The resume command must carry the settings the turn was dispatched with:
// a follow-up that silently drops --allow-write turns a write turn read-only,
// and one without --cwd runs against whatever tree the caller happens to be in.
func TestResumeCommandCarriesDispatchSettings(t *testing.T) {
	got := Turn{
		Provider:   "codex",
		SessionID:  "tid-1",
		Cwd:        "/repo",
		Model:      "gpt-5.3",
		Effort:     "xhigh",
		AllowWrite: true,
		TimeoutMin: 180,
	}.ResumeCommand()

	for _, want := range []string{
		"envoy turn", "--provider codex", "--resume tid-1", "--model gpt-5.3",
		"--effort xhigh", "--allow-write", "--cwd '/repo'", "--timeout-min 180",
		"--prompt-file <your-follow-up.md>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("resume command %q is missing %q", got, want)
		}
	}
	if (Turn{Provider: "claude"}).ResumeCommand() != "" {
		t.Fatal("no session id means no resume command")
	}
	// An omitted model must stay omitted — the engine never fills one in.
	if cmd := (Turn{Provider: "claude", SessionID: "s1", TimeoutMin: 30}).ResumeCommand(); strings.Contains(cmd, "--model") {
		t.Fatalf("resume command invented a model: %q", cmd)
	}
}

// Every prompt state has to yield a prescription, and only "accepted" and
// "unknown" may offer the resume path: prescribing resume after a provider
// that never started sends the caller down a session that does not exist.
func TestRecoveryCoversEveryPromptState(t *testing.T) {
	const resume = "envoy turn --provider claude --resume s1 --timeout-min 30 --prompt-file <your-follow-up.md>"
	cases := []struct {
		state       string
		wantResume  bool
		wantPhrases []string
	}{
		{job.PromptAccepted, true, []string{"accepted this prompt", "would repeat work"}},
		{job.PromptNotStarted, false, []string{"never started", "Re-run the identical command once"}},
		{job.PromptUnknown, true, []string{"unproven", "silence is not proof"}},
	}
	for _, c := range cases {
		got := Recovery(c.state, resume, "")
		if strings.TrimSpace(got) == "" {
			t.Fatalf("%s: no prescription", c.state)
		}
		if strings.Contains(got, resume) != c.wantResume {
			t.Fatalf("%s: resume offered = %v, want %v\n%s", c.state, !c.wantResume, c.wantResume, got)
		}
		for _, phrase := range c.wantPhrases {
			if !strings.Contains(got, phrase) {
				t.Fatalf("%s: prescription is missing %q:\n%s", c.state, phrase, got)
			}
		}
	}

	// A cause-specific fix rides along with the prescription.
	if got := Recovery(job.PromptAccepted, resume, "Raise the budget cap first."); !strings.Contains(got, "Raise the budget cap first.") {
		t.Fatalf("remedy dropped: %s", got)
	}
	// Without a session, the prescriptions stay honest about what is possible.
	if got := Recovery(job.PromptUnknown, "", ""); strings.Contains(got, "envoy turn") {
		t.Fatalf("no session must mean no resume command: %s", got)
	}
}

func TestStatusLineGlossesNonOK(t *testing.T) {
	if got := StatusLine(job.StatusOK); got != "ok" {
		t.Fatalf("ok needs no gloss, got %q", got)
	}
	for _, status := range []string{job.StatusFailed, job.StatusInfra, job.StatusTimeout, job.StatusInterrupted, job.StatusAbandoned} {
		got := StatusLine(status)
		if !strings.HasPrefix(got, status+" — ") {
			t.Fatalf("%s must carry a gloss, got %q", status, got)
		}
	}
}

// A fan-out's aggregate word has to carry how many results exist, because the
// caller acts on it before reading any member: "partial" that read like a
// failure would send it re-dispatching turns that already returned answers.
func TestFanStatusLineSaysHowManyResultsExist(t *testing.T) {
	cases := []struct {
		statuses []string
		status   string
		wants    []string
	}{
		{[]string{job.StatusOK, job.StatusOK}, FanOK, []string{"ok — all 2 turns returned a result"}},
		{[]string{job.StatusOK, job.StatusTimeout}, FanPartial,
			[]string{"partial — 1 of 2 turns returned a result", "their own status and next action below"}},
		{[]string{job.StatusTimeout, job.StatusFailed}, FanNoResult,
			[]string{"none of the 2 turns returned a result"}},
		{[]string{job.StatusRunning, job.StatusOK}, FanRunning,
			[]string{"1 of 2 turns have not finished", "nothing here is final yet"}},
		// A member that never published a status counts as one with no result;
		// claiming otherwise would license reading a result that is not there.
		{[]string{job.StatusOK, ""}, FanPartial, []string{"partial — 1 of 2 turns returned a result"}},
	}
	for _, c := range cases {
		if got := FanStatus(c.statuses); got != c.status {
			t.Errorf("FanStatus(%v) = %q, want %q", c.statuses, got, c.status)
		}
		line := FanStatusLine(c.statuses)
		for _, want := range c.wants {
			if !strings.Contains(line, want) {
				t.Errorf("FanStatusLine(%v) = %q, missing %q", c.statuses, line, want)
			}
		}
	}
}

// Members are independent turns, so the one thing a fan-out's closing line must
// never license is a group-wide re-dispatch: that would re-send a prompt other
// members already accepted.
func TestFanNextPrescribesPerMemberRecovery(t *testing.T) {
	mixed := FanNext("/jobs/g", []string{job.StatusOK, job.StatusTimeout})
	for _, want := range []string{
		"envoy collect '/jobs/g'",
		"licenses nothing about another",
		"re-dispatch or resume per member, never the whole fan-out",
	} {
		if !strings.Contains(mixed, want) {
			t.Errorf("FanNext for a partial fan-out %q is missing %q", mixed, want)
		}
	}
	none := FanNext("/jobs/g", []string{job.StatusTimeout, job.StatusInfra})
	if !strings.Contains(none, "prompt was accepted is what decides") {
		t.Errorf("FanNext with no results must name the prompt state as the discriminator, got %q", none)
	}
}

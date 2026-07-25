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

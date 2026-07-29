package prose

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

// The one case where the prompt-file placeholder closes is a steer supplement.
// The filled command is rendered from the turn's structured fields, never by
// editing the placeholder out of a finished string — so a dispatched path that
// happens to contain the placeholder text cannot collide with the slot.
func TestResumeCommandWith(t *testing.T) {
	turn := Turn{Provider: "codex", SessionID: "s1", Cwd: "/tmp/<your-follow-up.md>/repo", TimeoutMin: 30}
	got := turn.ResumeCommandWith("/tmp/supp file.md")
	if !strings.Contains(got, "--cwd '/tmp/<your-follow-up.md>/repo'") {
		t.Fatalf("cwd corrupted: %q", got)
	}
	if !strings.Contains(got, "--prompt-file '/tmp/supp file.md'") {
		t.Fatalf("prompt slot not filled or unquoted: %q", got)
	}
	if (Turn{Provider: "codex"}).ResumeCommandWith("/tmp/s.md") != "" {
		t.Fatal("no session id means no follow-up command, filled or not")
	}
	round := FanResumeCommandWith("/jobs/<your-follow-up.md>", 30, "/tmp/s.md")
	if !strings.Contains(round, "--resume-from '/jobs/<your-follow-up.md>'") ||
		!strings.Contains(round, "--prompt-file '/tmp/s.md'") {
		t.Fatalf("fan round rendered wrong: %q", round)
	}
}

// Every refusal of a job-dir continuation must carry a runnable next step —
// the reader is an agent whose next move is a command, not a diagnosis — and
// may prescribe only what the blocker actually observed: a running job gets
// "wait", never a resume beside a possibly live turn.
func TestResumeFromVocabulary(t *testing.T) {
	if got := TurnResumeFromCommand("/jobs/consult"); got != "envoy turn --resume-from '/jobs/consult' --prompt-file <your-follow-up.md>" {
		t.Fatalf("TurnResumeFromCommand = %q", got)
	}

	blocked := map[ResumeBlockerKind][]string{
		BlockerRunning:      {"still records status running", "one live turn at a time", "wait for its process to exit"},
		BlockerLockConflict: {"session-lock conflict", "may belong to another job"},
		BlockerNoSession:    {"never published a session id", "no conversation to continue", "fresh dispatch"},
	}
	for kind, wants := range blocked {
		got := ResumeFromBlocked("--resume-from", "/jobs/j1", kind)
		for _, want := range append(wants, "--resume-from /jobs/j1", "envoy collect '/jobs/j1'") {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: %q is missing %q", kind, got, want)
			}
		}
	}

	fanned := ResumeFromIsFanOut("--resume-from", "/jobs/fan", "envoy fan --resume-from '/jobs/fan' --timeout-min 30 --prompt-file <your-follow-up.md>")
	for _, want := range []string{"this is a fan-out", "envoy fan --resume-from '/jobs/fan'", "one member's directory"} {
		if !strings.Contains(fanned, want) {
			t.Fatalf("fan redirect %q is missing %q", fanned, want)
		}
	}

	single := FanSingleWithFrom("/jobs/consult")
	if !strings.Contains(single, "envoy turn --resume-from '/jobs/consult' --prompt-file <your-follow-up.md>") {
		t.Fatalf("single with-from must hand over the runnable turn form: %q", single)
	}

	dup := FanWithFromDuplicateSession("s1", "/jobs/a", "/jobs/b")
	for _, want := range []string{"same conversation twice", "session s1", "/jobs/a", "/jobs/b", "one live turn at a time"} {
		if !strings.Contains(dup, want) {
			t.Fatalf("duplicate-session refusal %q is missing %q", dup, want)
		}
	}
}

// Steer's whole vocabulary answers one question — can a supplement still reach
// this job? — and the honest answer is always no. Each wording must say why in
// the provider's own terms and hand over a runnable continuation, and the
// non-ok terminal wording must never prescribe a resume the job's recovery has
// not licensed.
func TestSteerVocabulary(t *testing.T) {
	const filled = "envoy turn --provider claude --resume s1 --timeout-min 30 --prompt-file '/tmp/supp.md'"

	live := SteerLive("claude", filled, "/jobs/j1")
	if !strings.Contains(live.Why, "claude takes no input into a turn in flight") ||
		!strings.Contains(live.Why, "queue the supplement as a second turn") {
		t.Fatalf("claude live why = %q", live.Why)
	}
	if !strings.Contains(live.Next, filled) {
		t.Fatalf("claude live next must hand over the filled command: %q", live.Next)
	}

	codexLive := SteerLive("codex", filled, "/jobs/j1")
	if !strings.Contains(codexLive.Why, "codex takes no input after dispatch") {
		t.Fatalf("codex live why = %q", codexLive.Why)
	}

	// No session yet: nothing runnable exists, and inventing one is forbidden —
	// the caller is pointed at collect, which prints the command once it exists.
	noSession := SteerLive("codex", "", "/jobs/j1")
	if strings.Contains(noSession.Next, "envoy turn") {
		t.Fatalf("no session must mean no resume command: %q", noSession.Next)
	}
	if !strings.Contains(noSession.Next, "envoy collect '/jobs/j1'") {
		t.Fatalf("no-session next must point at collect: %q", noSession.Next)
	}

	stale := SteerStale("abandoned", "/jobs/j1")
	if !strings.Contains(stale.Why, "runner process is gone (abandoned)") ||
		!strings.Contains(stale.Next, "envoy collect '/jobs/j1'") {
		t.Fatalf("stale = %+v", stale)
	}

	okFresh := SteerTerminalOK(filled, "/jobs/j1", false)
	if !strings.Contains(okFresh.Next, "read the result first") || !strings.Contains(okFresh.Next, filled) {
		t.Fatalf("uncollected ok next = %q", okFresh.Next)
	}
	okCollected := SteerTerminalOK(filled, "/jobs/j1", true)
	if strings.Contains(okCollected.Next, "read the result first") || !strings.Contains(okCollected.Next, filled) {
		t.Fatalf("collected ok next = %q", okCollected.Next)
	}

	failed := SteerTerminalNotOK(job.StatusFailed, "/jobs/j1")
	if strings.Contains(failed.Next, "envoy turn") {
		t.Fatalf("a non-ok terminal steer must not prescribe a resume: %q", failed.Next)
	}
	if !strings.Contains(failed.Why, "status failed") || !strings.Contains(failed.Next, "envoy collect '/jobs/j1'") {
		t.Fatalf("failed = %+v", failed)
	}

	group := SteerGroup(
		[]string{SteerCommand("/tmp/supp.md", "/jobs/fan/codex"), SteerCommand("/tmp/supp.md", "/jobs/fan/claude")},
		"envoy fan --resume-from '/jobs/fan' --timeout-min 30 --prompt-file '/tmp/supp.md'")
	for _, want := range []string{
		"envoy steer --prompt-file '/tmp/supp.md' '/jobs/fan/codex'",
		"envoy steer --prompt-file '/tmp/supp.md' '/jobs/fan/claude'",
		"envoy fan --resume-from '/jobs/fan'",
	} {
		if !strings.Contains(group.Next, want) {
			t.Fatalf("group next missing %q:\n%s", want, group.Next)
		}
	}
}

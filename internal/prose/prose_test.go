package prose

import (
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
)

func TestContextUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    *job.Usage
		want string
	}{
		{"unsupported", nil, ""},
		{"unmeasured", job.NewUsage(), "context: no measurement yet"},
		{"live", &job.Usage{LatestContextTokens: job.Ptr(int64(138000)), PeakContextTokens: job.Ptr(int64(210000)), Responses: 1},
			"context: 138k, window unknown, peak 210k, 1 response"},
		{"settled", &job.Usage{Final: true, State: "settled", LatestContextTokens: job.Ptr(int64(210000)), PeakContextTokens: job.Ptr(int64(210000)), ContextWindowTokens: job.Ptr(int64(1000000)), Responses: 58},
			"context: 210k of 1M (21%), peak 210k, 58 responses"},
		{"missing terminal", &job.Usage{Final: true, State: "incomplete", LatestContextTokens: job.Ptr(int64(138000)), Issues: []string{"missing_terminal"}},
			"context: 138k, window unknown, 0 responses; incomplete (no terminal record)"},
		{"unattributed", &job.Usage{Issues: []string{"model_mismatch"}, UnexpectedModel: job.Ptr("other\nmodel")},
			`context: no measurement yet; incomplete (unexpected model "other\nmodel")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContextUsage(tc.u); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A cap alone reads the same whether the provider worked until the deadline or
// went silent seconds after accepting the prompt, and those two turns want
// opposite follow-ups: one continues work, the other is addressed to a session
// that shows none. The engine records the difference, so the envelope must say
// it — without ever calling the turn hung, which the cap does not prove.
func TestTimedOutCarriesTheStreamItObserved(t *testing.T) {
	quiet := TimedOut(30, "codex", CapStream{Events: 2, LastEvent: "turn.started", Quiet: 29*time.Minute + 58*time.Second})
	for _, want := range []string{
		"The 30-minute wall-clock cap ended this codex turn.",
		"not evidence the provider hung",
		"the stream had been quiet for 29m, after 2 events (last: turn.started)",
	} {
		if !strings.Contains(quiet, want) {
			t.Fatalf("capped-and-quiet envelope %q is missing %q", quiet, want)
		}
	}

	busy := TimedOut(60, "claude", CapStream{Events: 431, LastEvent: "assistant", Quiet: 3 * time.Second})
	if !strings.Contains(busy, "quiet for 3s, after 431 events (last: assistant)") {
		t.Fatalf("a turn still streaming at the cap must say so: %q", busy)
	}

	silent := TimedOut(30, "codex", CapStream{})
	if !strings.Contains(silent, "wrote nothing at all before the cap") {
		t.Fatalf("a turn with no events must say so plainly: %q", silent)
	}
	if strings.Contains(silent, "quiet for") {
		t.Fatalf("no output means no quiet interval to report: %q", silent)
	}

	// Bytes are not events. A provider that writes a diagnostic and then never
	// emits anything parseable did stream something, and "nothing at all"
	// there is simply false.
	noisy := TimedOut(30, "codex", CapStream{Bytes: 157})
	if !strings.Contains(noisy, "wrote 157 bytes before the cap but no event envoy could parse") {
		t.Fatalf("bytes without events must be reported as such: %q", noisy)
	}
	if strings.Contains(noisy, "nothing at all") {
		t.Fatalf("output arrived, so nothing-at-all is false: %q", noisy)
	}

	// The clause reports; it never concludes no work happened. That verdict
	// belongs to the recovery line, which prompt state governs.
	for _, envelope := range []string{quiet, busy, silent, noisy} {
		if strings.Contains(envelope, "no work") {
			t.Fatalf("the stream clause may not rule out work: %q", envelope)
		}
	}
	// The observation is evidence for the caller, never a verdict the engine
	// acts on: the cap stays a deadline, so no wording may call the turn hung.
	for _, envelope := range []string{quiet, busy, silent, noisy} {
		if strings.Contains(envelope, "stalled") || strings.Contains(envelope, "stuck") {
			t.Fatalf("the envelope must not diagnose a stall: %q", envelope)
		}
		if strings.Count(envelope, "hung") != strings.Count(envelope, "not evidence the provider hung") {
			t.Fatalf("the only mention of hanging may be the one ruling it out: %q", envelope)
		}
	}
}

// A re-dispatch repeats the dispatch it replaces. A continuation names its
// source again — a cold voice would start a different conversation — and
// every recorded CLI setting rides along, the spend cap included; a model that
// was omitted stays omitted.
func TestRedispatchRepeatsTheDispatch(t *testing.T) {
	warm := &job.Meta{Provider: "claude", Model: job.Ptr("opus"), ResumedFrom: job.Ptr("/jobs/consult-r1"),
		Cwd: "/repo", GitBaseline: job.Ptr("abc123"), MaxBudgetUSD: job.Ptr(0.25), TimeoutMin: 9}
	got := RedispatchCommand("/jobs/review", warm)
	for _, want := range []string{
		"envoy run <new-job-name>", "--with @'/jobs/consult-r1'", "--prompt-file '/jobs/review/prompt.md'",
		"--baseline abc123", "--cwd '/repo'", "--max-budget-usd 0.25", "--timeout-min 9",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("redispatch command %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "--with claude") {
		t.Fatalf("a continuation's retry must not become a cold voice: %q", got)
	}
	cold := &job.Meta{Provider: "codex", Effort: job.Ptr("xhigh"), AllowWrite: true, TimeoutMin: 180}
	got = RedispatchCommand("/jobs/delegate", cold)
	for _, want := range []string{"--with codex::xhigh", "--allow-write", "--timeout-min 180"} {
		if !strings.Contains(got, want) {
			t.Fatalf("redispatch command %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "--max-budget-usd") || strings.Contains(got, "--baseline") {
		t.Fatalf("redispatch command invented a setting: %q", got)
	}
	if got := voiceSpec("codex", "", "high"); got != "codex::high" {
		t.Fatalf("effort without a model = %q, want the empty model slot kept", got)
	}
}

// A reported model is shown only beside a request it does not spell, and an
// omitted request always reads as the provider's own default.
func TestModelSettingShowsTheReportOnlyWhenItDiffers(t *testing.T) {
	cases := []struct{ requested, reported, want string }{
		{"", "", "(provider default)"},
		{"opus", "", "opus"},
		{"opus", "opus", "opus"},
		{"", "claude-opus-5", "(provider default, ran claude-opus-5)"},
		{"opus", "claude-opus-5", "opus (ran claude-opus-5)"},
	}
	for _, c := range cases {
		if got := ModelSetting(c.requested, c.reported); got != c.want {
			t.Errorf("ModelSetting(%q, %q) = %q, want %q", c.requested, c.reported, got, c.want)
		}
	}
}

// Every prompt state has to yield a prescription, and only "accepted" and
// "unknown" may offer the resume path: prescribing resume after a provider
// that never started sends the caller down a session that does not exist.
func TestRecoveryCoversEveryPromptState(t *testing.T) {
	const resume = "envoy run <new-job-name> --with @'/jobs/j1' --timeout-min 30 --prompt-file <your-follow-up.md>"
	const redispatch = "envoy run <new-job-name> --with claude --prompt-file '/jobs/j1/prompt.md' --timeout-min 30"
	cases := []struct {
		state          string
		wantResume     bool
		wantRedispatch bool
		wantPhrases    []string
	}{
		{job.PromptAccepted, true, false, []string{"accepted this prompt", "would repeat work"}},
		{job.PromptNotStarted, false, true, []string{"never started", "new job name", "keeps this record collectable by name"}},
		{job.PromptUnknown, true, false, []string{"unproven", "silence is not proof"}},
	}
	for _, c := range cases {
		got := Recovery(&job.Meta{PromptState: c.state}, resume, redispatch)
		if strings.TrimSpace(got) == "" {
			t.Fatalf("%s: no prescription", c.state)
		}
		if strings.Contains(got, resume) != c.wantResume {
			t.Fatalf("%s: resume offered = %v, want %v\n%s", c.state, !c.wantResume, c.wantResume, got)
		}
		if strings.Contains(got, redispatch) != c.wantRedispatch {
			t.Fatalf("%s: redispatch offered = %v, want %v\n%s", c.state, !c.wantRedispatch, c.wantRedispatch, got)
		}
		for _, phrase := range c.wantPhrases {
			if !strings.Contains(got, phrase) {
				t.Fatalf("%s: prescription is missing %q:\n%s", c.state, phrase, got)
			}
		}
	}

	// A cause-specific fix rides along with the prescription.
	if got := Recovery(&job.Meta{PromptState: job.PromptAccepted, Remedy: job.Ptr("Raise the budget cap first.")}, resume, redispatch); !strings.Contains(got, "Raise the budget cap first.") {
		t.Fatalf("remedy dropped: %s", got)
	}
	// Without a session, the prescriptions stay honest about what is possible.
	if got := Recovery(&job.Meta{PromptState: job.PromptUnknown}, "", ""); strings.Contains(got, "envoy run") {
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
		{[]string{job.StatusOK, job.StatusTimeout}, fanPartial,
			[]string{"partial — 1 of 2 turns returned a result", "their own status and next action below"}},
		{[]string{job.StatusTimeout, job.StatusFailed}, fanNoResult,
			[]string{"none of the 2 turns returned a result"}},
		{[]string{job.StatusRunning, job.StatusOK}, fanRunning,
			[]string{"1 of 2 turns have not finished", "nothing here is final yet"}},
		// A member that never published a status counts as one with no result;
		// claiming otherwise would license reading a result that is not there.
		{[]string{job.StatusOK, ""}, fanPartial, []string{"partial — 1 of 2 turns returned a result"}},
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
// Every refusal of a continuation must carry a runnable next step — the
// reader is an agent whose next move is a command, not a diagnosis — and may
// prescribe only what the blocker actually observed: a running job gets
// "wait", never a resume beside a possibly live turn.
func TestContinueVocabulary(t *testing.T) {
	if got, want := ContinueCommand("/jobs/consult", 30), "envoy run <new-job-name> --with @'/jobs/consult' --timeout-min 30 --prompt-file <your-follow-up.md>"; got != want {
		t.Fatalf("ContinueCommand = %q, want %q", got, want)
	}

	blocked := map[ResumeBlockerKind][]string{
		BlockerRunning:      {"still records status running", "one live turn at a time", "wait for its process to exit"},
		BlockerLockConflict: {"session-lock conflict", "may belong to another job"},
		BlockerNoSession:    {"never published a session id", "no conversation to continue", "fresh dispatch"},
	}
	for kind, wants := range blocked {
		got := ContinueBlocked("/jobs/j1", kind)
		for _, want := range append(wants, "--with @/jobs/j1", "envoy collect '/jobs/j1'") {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: %q is missing %q", kind, got, want)
			}
		}
	}

	// A fan-out reference beside other voices: copy-ready member voices for
	// the eligible members only, and the blocked one named as an observation.
	members := []FanMemberCandidate{
		{Name: "codex", Dir: "/jobs/fan/codex"},
		{Name: "claude-opus", Dir: "/jobs/fan/claude-opus", Kind: BlockerRunning},
	}
	mixed := GroupRefMustStandAlone("/jobs/fan", members)
	for _, want := range []string{"names a fan-out", "stands alone", "--with @'/jobs/fan/codex'  (codex)", "member claude-opus is still running"} {
		if !strings.Contains(mixed, want) {
			t.Fatalf("group-ref refusal %q is missing %q", mixed, want)
		}
	}
	if strings.Contains(mixed, "--with @'/jobs/fan/claude-opus'") {
		t.Fatalf("group-ref refusal must offer only eligible members: %q", mixed)
	}

	dup := DuplicateConversation("s1", "/jobs/a", "/jobs/b")
	for _, want := range []string{"same conversation twice", "session s1", "/jobs/a", "/jobs/b", "one live turn at a time"} {
		if !strings.Contains(dup, want) {
			t.Fatalf("duplicate-session refusal %q is missing %q", dup, want)
		}
	}

	// A taken path is refused with the way to read what holds it, and a
	// write conversation is kept whole by continuing alone.
	if got := JobExists("/jobs/r1"); !strings.Contains(got, "a directory holds one job") || !strings.Contains(got, "envoy collect '/jobs/r1'") {
		t.Fatalf("JobExists = %q", got)
	}
	// A held name says nothing ran, that collecting the name reads the other
	// job, and what frees it — with the collect command only where collecting
	// is what frees it, and removal only of a directory seen to be empty.
	for _, c := range []struct {
		hold NameHold
		want []string
		not  []string
	}{
		{NameHold{Kind: HoldRunning}, []string{"which is still running", "its own caller has collected it"}, []string{"envoy collect", "removing"}},
		{NameHold{Kind: HoldRunning, Member: "codex"}, []string{"whose member codex is still running"}, []string{"envoy collect"}},
		{NameHold{Kind: HoldUncollected}, []string{"has not been collected", "yours or its caller is gone", "envoy collect '/jobs/review-r1+2'"}, []string{"removing"}},
		{NameHold{Kind: HoldUnrecorded, Empty: true}, []string{"holds no record", "nothing here says which", "established that no envoy run owns", "still empty, removing it"}, []string{"envoy collect", "minute"}},
		{NameHold{Kind: HoldUnrecorded}, []string{"holds no record", "Read what that directory holds"}, []string{"envoy collect", "removing", "empty", "minute"}},
		{NameHold{Kind: HoldUnrecorded, Member: "codex"}, []string{"whose member codex has written no record", "may still be alive", "its own caller has collected it"}, []string{"envoy collect", "removing", "minute"}},
		{NameHold{Kind: HoldUnreadable}, []string{"cannot be read", "Inspect that directory"}, []string{"envoy collect", "removing"}},
	} {
		got := NameHeld("review-r1", "/jobs/review-r1+2", c.hold)
		// Only a job that can be read is described as readable: a directory
		// with no record, or an unreadable one, makes no claim about collect.
		readable := c.hold.Kind == HoldRunning || c.hold.Kind == HoldUncollected || c.hold.Member != ""
		if strings.Contains(got, "now reads an earlier job, not a new one") != readable {
			t.Fatalf("NameHeld(%+v) = %q: the collect claim must appear exactly when a job can be read", c.hold, got)
		}
		if strings.Contains(got, "reads that job") {
			t.Fatalf("NameHeld(%+v) = %q: the holder is not always what the name reads", c.hold, got)
		}
		for _, w := range append(c.want, "nothing was dispatched", "/jobs/review-r1+2", "review-r1-b") {
			if !strings.Contains(got, w) {
				t.Fatalf("NameHeld(%+v) = %q, missing %q", c.hold, got, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Fatalf("NameHeld(%+v) = %q, must not say %q", c.hold, got, n)
			}
		}
	}
	if got := WriteSourceInRoster("/jobs/w", 60); !strings.Contains(got, "--allow-write") || !strings.Contains(got, ContinueCommand("/jobs/w", 60)) {
		t.Fatalf("WriteSourceInRoster = %q", got)
	}
}

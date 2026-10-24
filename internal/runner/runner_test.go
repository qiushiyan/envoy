package runner

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/provider"
)

// The hard cap must be a wall-clock deadline: Go's monotonic readings freeze
// during laptop sleep (mach_absolute_time on darwin), so a deadline carrying
// one silently stretches the cap across a suspend — the exact failure the JS
// engine's Date.now() comparison was built to prevent. A time.Time formats
// with an " m=±…" suffix iff it still carries a monotonic reading.
func TestDeadlineIsWallClock(t *testing.T) {
	d := deadlineFrom(time.Now(), 30)
	if strings.Contains(d.String(), " m=") {
		t.Fatalf("deadline carries a monotonic reading; laptop sleep would stretch the cap: %v", d)
	}
	if !deadlineFrom(time.Now(), 0).IsZero() {
		t.Fatal("timeout 0 must mean no deadline")
	}
	want := 90 * time.Second
	got := deadlineFrom(time.Now(), 1.5).Sub(time.Now().Round(0)).Round(time.Second)
	if got != want {
		t.Fatalf("deadline offset = %v, want %v", got, want)
	}
}

func usageRun(t *testing.T) (*run, []string) {
	t.Helper()
	ws := job.Workspace{Dir: t.TempDir()}
	started := time.Now()
	driver, err := provider.New("claude", provider.Options{}, ws, started)
	if err != nil {
		t.Fatal(err)
	}
	r := &run{ws: ws, driver: driver, startedAt: started, progress: ws.Progress(),
		opts: Options{Provider: "claude", Stdout: io.Discard, Stderr: io.Discard}}
	r.initMeta()
	r.writeMeta(nil)
	data, err := os.ReadFile("../provider/testdata/claude-2.1.270-usage.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return r, strings.Split(strings.TrimSpace(string(data)), "\n")
}

func diskUsage(t *testing.T, r *run) *job.Meta {
	t.Helper()
	m, err := job.ReadMeta(r.ws.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The terminal envelope's settled usage reaches meta.json when the envelope
// arrives, not when the process is finalized: a runner killed between the two
// still leaves the settled measurement. Live persistence of each response is
// tests/usage_test.go's.
func TestSettledUsageIsPersistedBeforeTheTurnIsFinished(t *testing.T) {
	r, records := usageRun(t)
	for _, line := range records {
		r.handleEvents(r.driver.Feed(line))
	}
	m := diskUsage(t, r)
	if m.Status != job.StatusRunning || m.Usage.State != "settled" || *m.Usage.LatestContextTokens != 19454 || *m.Usage.OutputTokens != 338 {
		t.Fatalf("terminal envelope not persisted before the turn finished: status=%s usage=%+v", m.Status, m.Usage)
	}
}

func TestUsageFinalizationWithoutTerminal(t *testing.T) {
	for _, status := range []string{job.StatusInfra, job.StatusTimeout, job.StatusInterrupted} {
		t.Run(status, func(t *testing.T) {
			r, records := usageRun(t)
			for _, line := range records[:2] {
				r.handleEvents(r.driver.Feed(line))
			}
			r.finish(provider.Outcome{Status: status, Failure: &job.Failure{Cause: job.CauseInterrupted}}, exitResult{})
			u := diskUsage(t, r).Usage
			if !u.Final || u.State != "incomplete" || *u.LatestContextTokens != 18889 || *u.InputTokens != 2 || u.OutputTokens != nil || !slices.Contains(u.Issues, "missing_terminal") {
				t.Fatalf("final usage = %+v", u)
			}
		})
	}
}

// Acceptance proved only at the end — a transcript, a recovered output file —
// is recorded the way a streamed proof is: its time and its progress line
// too, never just a state flipped in the final record. And a proof that
// arrives after the stream's own keeps the stream's evidence.
func TestAnEndingThatProvesAcceptanceStampsItLikeTheStream(t *testing.T) {
	r, _ := usageRun(t)
	r.finish(provider.Outcome{Status: job.StatusFailed, Failure: &job.Failure{Cause: job.CauseProviderVerdict},
		PromptState: job.PromptAccepted, Evidence: "claude session transcript"}, exitResult{})
	m := diskUsage(t, r)
	if m.PromptState != job.PromptAccepted || m.PromptAcceptedAt == nil ||
		job.Deref(m.PromptStateEvidence) != "claude session transcript" {
		t.Fatalf("late acceptance = state %s at %v evidence %v", m.PromptState, m.PromptAcceptedAt, m.PromptStateEvidence)
	}
	progress, _ := os.ReadFile(r.ws.ProgressLogPath())
	if !strings.Contains(string(progress), "state=accepted") {
		t.Fatalf("late acceptance left no progress line:\n%s", progress)
	}

	r, _ = usageRun(t)
	r.markPromptAccepted("claude assistant")
	r.finish(provider.Outcome{Status: job.StatusOK, PromptState: job.PromptAccepted, Evidence: "claude result/success"}, exitResult{})
	if got := job.Deref(diskUsage(t, r).PromptStateEvidence); got != "claude assistant" {
		t.Fatalf("an accepted turn keeps its first evidence, got %q", got)
	}
}

// A stop escalates once: residual cleanup and a repeated cap ride the stop
// already under way instead of pushing its SIGKILL later, and an interrupt
// arriving during it leaves the stop the reason it started with. What that
// interrupt does to the tree is TestASecondInterruptKillsTheTreeNow's.
func TestAStopEscalatesOnceAndKeepsItsReason(t *testing.T) {
	r, _ := usageRun(t)
	r.requestTermination(stopTimeout, nil)
	if r.escalation != terminating || r.escalationTimer == nil {
		t.Fatalf("a stop must schedule SIGKILL: escalation %v", r.escalation)
	}
	pendingKill := r.escalationTimer
	r.cleanupResidual()
	r.requestTermination(stopTimeout, nil)
	if r.escalation != terminating || r.escalationTimer != pendingKill {
		t.Fatal("a stop already under way keeps the SIGKILL deadline it set")
	}
	r.requestTermination(stopInterrupted, syscall.SIGINT)
	r.escalationTimer.Stop()
	if r.term.reason != stopTimeout || diskUsage(t, r).InterruptionSignal != nil {
		t.Fatalf("the stop keeps the reason it started with: %+v", r.term)
	}
}

// A turn whose standing instructions cannot be archived never spawns: every
// later turn of its conversation passes them again from that archive, so a
// turn run without one would leave the next turn nothing to pass.
func TestATurnWhoseInstructionsCannotBeArchivedNeverSpawns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	marker := filepath.Join(t.TempDir(), "spawned")
	launcher := filepath.Join(t.TempDir(), "launcher")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENVOY_CODEX_CMD", launcher)
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(prompt, []byte("the turn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	// A directory where the archive goes: the write fails, nothing else does.
	if err := os.Mkdir(job.Workspace{Dir: out}.InstructionsPath(), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	res := Run(Options{Provider: "codex", PromptFile: prompt, Cwd: t.TempDir(), OutDir: out,
		Turn: provider.Options{Instructions: "rule A\n"}, Stdout: &stdout, Stderr: &stderr})
	if res.ExitCode != job.ExitInfra || !strings.Contains(stderr.String(), "cannot prepare job dir") {
		t.Fatalf("Run = %+v, stderr %q; want infra before anything ran", res, stderr.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the provider was launched without its instructions archive")
	}
}

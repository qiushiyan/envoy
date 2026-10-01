package runner

import (
	"io"
	"os"
	"slices"
	"strings"
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
	m, err := job.ReadMetaFile(r.ws.MetaPath())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUsageIsPersistedDuringTheTurn(t *testing.T) {
	r, records := usageRun(t)
	if m := diskUsage(t, r); m.Status != job.StatusRunning || m.Usage.State != "unmeasured" {
		t.Fatalf("initial record = %+v", m)
	}
	wantResponses := []int64{0, 1, 1, 2, 3, 3}
	for i, line := range records {
		r.handleEvents(r.driver.Feed(line))
		// No heartbeat or process finalization: the events alone must publish
		// every changed sample, including responses after first acceptance.
		m := diskUsage(t, r)
		if m.Status != job.StatusRunning || m.Tokens != nil || m.Usage.Responses != wantResponses[i] {
			t.Fatalf("record %d: status=%s tokens=%+v usage=%+v", i, m.Status, m.Tokens, m.Usage)
		}
		if i > 0 && i < len(records)-1 && (m.Usage.State != "live" || m.Usage.OutputTokens != nil || m.Usage.ContextWindowTokens != nil) {
			t.Fatalf("record %d fabricated settled usage: %+v", i, m.Usage)
		}
	}
	m := diskUsage(t, r)
	if m.Usage.State != "settled" || *m.Usage.LatestContextTokens != 19454 || *m.Usage.OutputTokens != 338 {
		t.Fatalf("terminal envelope not persisted before process exit: %+v", m.Usage)
	}
	out := r.driver.Conclude(provider.ExitInfo{Code: job.Ptr(0)})
	r.finish(out, exitResult{})
	m = diskUsage(t, r)
	if m.Status != job.StatusOK || m.Usage.State != "settled" || *m.Tokens.Output != 338 {
		t.Fatalf("finish changed settled usage or tokens writer: %+v", m)
	}
}

func TestUsageFinalizationWithoutTerminal(t *testing.T) {
	for _, status := range []string{job.StatusInfra, job.StatusTimeout, job.StatusInterrupted} {
		t.Run(status, func(t *testing.T) {
			r, records := usageRun(t)
			for _, line := range records[:2] {
				r.handleEvents(r.driver.Feed(line))
			}
			r.finish(provider.Outcome{Status: status, ErrorText: "test stop"}, exitResult{})
			u := diskUsage(t, r).Usage
			if !u.Final || u.State != "incomplete" || *u.LatestContextTokens != 18889 || *u.InputTokens != 2 || u.OutputTokens != nil || !slices.Contains(u.Issues, "missing_terminal") {
				t.Fatalf("final usage = %+v", u)
			}
		})
	}
}

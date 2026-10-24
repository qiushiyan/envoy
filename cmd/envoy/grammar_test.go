package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/clikit/clikittest"
	"github.com/qiushiyan/envoy/internal/detach"
)

// make install can replace the binary between an older waiter's start and its
// exec of __envoy-spawn, so the detached process of this version receives
// argv a waiter of the last one forwarded — unparsed, since that waiter never
// parsed it. Every shape a caller dispatches must still run there; the
// spellings this version refuses (a long option with one dash) are refused
// there too, by the detached process, with nothing dispatched.
func TestAnOlderWaitersArgvRunsInTheDetachedProcess(t *testing.T) {
	w := newWorld(t)
	brief := w.path("brief.md")
	for _, c := range []struct {
		job  string
		tail []string
		want int
	}{
		{"review-r1", []string{"review-r1", "--with", "codex:gpt-6-astra:high", "--prompt-file", brief, "--baseline", w.base, "--timeout-min", "60"}, 0},
		{"consult-r1", []string{"consult-r1", "--with", "codex:gpt-6-astra:high", "--with", "claude:claude-opus-5-5", "--prompt-file", brief, "--timeout-min", "30"}, 0},
		{"consult-r2", []string{"consult-r2", "--with", "claude:claude-fable-5-1=" + w.path("critique.md"), "--with", "codex:gpt-6-astra:high=" + w.path("survey.md")}, 0},
		{"review-r2", []string{"review-r2", "--with", "@consult-r1/codex-gpt-6-astra", "--with", "claude:claude-opus-5-5", "--prompt-file", brief, "--baseline", w.base}, 0},
		{"delegate-r1", []string{"delegate-r1", "--with", "codex:gpt-6.1-sol:xhigh", "--allow-write", "--baseline", w.base, "--prompt-file", brief, "--timeout-min", "180"}, 0},
		{w.path("steward/turn-1"), []string{w.path("steward/turn-1"), "--prompt-file", brief, "--with", "claude:claude-opus-5-5:high", "--allow-write", "--cwd", w.repo, "--timeout-min", "30", "--max-budget-usd", "0.50", "--instructions-file", w.path("rules.md")}, 0},
		{w.path("steward/turn-2"), []string{w.path("steward/turn-2"), "--prompt-file", w.path("next.md"), "--with", "@" + w.path("steward/turn-1"), "--allow-write", "--cwd", w.repo, "--timeout-min", "30"}, 0},
		{"dash-r1", []string{"dash-r1", "-with", "codex", "-prompt-file", brief}, 3},
	} {
		var out, errb strings.Builder
		code := detach.Dispatch(c.job, c.tail, &out, &errb)
		if code != c.want {
			t.Errorf("%v: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", c.tail, code, c.want, out.String(), errb.String())
			continue
		}
		dispatched := strings.Contains(out.String(), "job: ")
		if dispatched != (c.want == 0) {
			t.Errorf("%v: dispatched = %v\nstdout:\n%s\nstderr:\n%s", c.tail, dispatched, out.String(), errb.String())
		}
	}
	if _, err := os.Stat(filepath.Join(w.home, ".local", "state", "envoy", "jobs", "dash-r1")); err == nil {
		t.Error("the refused one-dash spelling left a job directory")
	}
}

// A refusal is printed once, under one prefix, with its body intact: the
// command line adds "envoy: error: " and nothing else, so a refusal that
// lists the members a caller can seat instead keeps its lines.
func TestARefusalKeepsItsLinesUnderOnePrefix(t *testing.T) {
	w := newWorld(t)
	brief := w.path("brief.md")
	if r := clikittest.Run(app, "run", "consult-r1", "--with", "codex", "--with", "claude:opus", "--prompt-file", brief); r.Code != 0 {
		t.Fatalf("consult-r1: exit %d\n%s", r.Code, r.Stderr)
	}
	r := clikittest.Run(app, "run", "review-r1", "--with", "@consult-r1", "--with", "claude", "--prompt-file", brief)
	if r.Code != 3 {
		t.Fatalf("exit %d, want 3\n%s", r.Code, r.Stderr)
	}
	lines := strings.Split(strings.TrimSuffix(r.Stderr, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "envoy: error: --with @") || !strings.Contains(lines[0], "stands alone as the only --with") {
		t.Fatalf("first line:\n%s", r.Stderr)
	}
	members := 0
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "envoy:") {
			t.Errorf("a second prefix inside the body:\n%s", r.Stderr)
		}
		if strings.HasPrefix(l, "  --with @") {
			members++
		}
	}
	if members != 2 {
		t.Errorf("want both members offered on lines of their own:\n%s", r.Stderr)
	}
}

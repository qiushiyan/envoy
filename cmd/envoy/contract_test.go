package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/qiushiyan/clikit/clikittest"
)

// The contract suite: what envoy's callers read, byte for byte. Each scenario
// runs the invocation shapes a caller uses — the consult, review and delegate
// skills' dispatch lines, the planlab steward's run argv (including the
// `lock error:` line it greps on exit 3), claude-steps' run, collect and block
// lines, `envoy pending` and `envoy version` — over a fixture world against
// the fake provider in tests/fake-bin, and pins stdout, stderr and the exit
// code of every step in testdata/contract. Help pages are TestHelpPages'.
//
// Record the files from a built binary — the old one, before a change that
// must keep its bytes:
//
//	ENVOY_CONTRACT_OLD=/abs/path/to/old/envoy go test ./cmd/envoy -run TestContracts
//
// Without the variable each step runs in process and must match its file
// byte for byte after normalise. An intended difference is re-recorded from
// the tree with ENVOY_CONTRACT_UPDATE=1, reviewed as a diff, and listed where
// the project keeps compatibility decisions.

var (
	// basePATH is the PATH the fake provider's directory is put in front of.
	basePATH string
	// pkgDir is this package's directory; scenarios chdir into their world.
	pkgDir string
)

type world struct {
	t          *testing.T
	home, repo string
	base       string // the fixture repository's commit, the baseline callers pass
}

// newWorld gives a scenario a temporary HOME holding a git repository with
// one commit, the prompt files the skills write, and the environment the
// binary-driven suite uses, with the session identity fixed.
func newWorld(t *testing.T) *world {
	t.Helper()
	home := t.TempDir()
	w := &world{t: t, home: home, repo: filepath.Join(home, "repo")}
	for k, v := range map[string]string{
		"HOME":                         home,
		"PATH":                         filepath.Join(pkgDir, "..", "..", "tests", "fake-bin") + ":" + basePATH,
		"ENVOY_HEARTBEAT_MS":           "100",
		"ENVOY_TIMEOUT_POLL_MS":        "25",
		"ENVOY_SIGKILL_AFTER_MS":       "400",
		"ENVOY_CLOSE_GRACE_MS":         "150",
		"ENVOY_CALLER":                 "contract-caller",
		"ENVOY_FAKE_SCENARIO":          "success",
		"ENVOY_FAKE_SESSION_ID_CODEX":  "sess-codex",
		"ENVOY_FAKE_SESSION_ID_CLAUDE": "sess-claude",
	} {
		t.Setenv(k, v)
	}
	if err := os.MkdirAll(w.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	w.write("repo/README.md", "fixture\n")
	w.git("init", "-q", "-b", "main")
	w.git("add", ".")
	w.git("commit", "-q", "-m", "fixture")
	w.base = strings.TrimSpace(w.git("rev-parse", "HEAD"))
	for name, body := range map[string]string{
		"brief.md":    "review the fixture\n",
		"critique.md": "critique the design\n",
		"survey.md":   "survey what exists\n",
		"full.md":     "the full brief\n",
		"goal.md":     "the goal brief\n",
		"round2.md":   "round two\n",
		"rules.md":    "work unattended\n",
		"next.md":     "the next turn\n",
	} {
		w.write(name, body)
	}
	t.Chdir(w.repo)
	return w
}

func (w *world) write(rel, body string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.home, rel), []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) path(rel string) string { return filepath.Join(w.home, rel) }

func (w *world) git(args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = w.repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com", "GIT_AUTHOR_DATE=2026-09-01T00:00:00Z",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com", "GIT_COMMITTER_DATE=2026-09-01T00:00:00Z",
		"GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// invoke runs one envoy command the way the scenario's caller does: in
// process through the same entry main uses, or, while recording, through the
// binary ENVOY_CONTRACT_OLD names.
func invoke(t *testing.T, args []string) (code int, stdout, stderr string) {
	t.Helper()
	if old := os.Getenv("ENVOY_CONTRACT_OLD"); old != "" {
		cmd := exec.Command(old, args...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("running %s: %v", old, err)
		}
		return code, out.String(), errb.String()
	}
	r := clikittest.Run(app, args...)
	return r.Code, r.Stdout, r.Stderr
}

// step runs one command and appends its transcript to the scenario's.
func (w *world) step(b *strings.Builder, env map[string]string, args ...string) int {
	w.t.Helper()
	for k, v := range env {
		w.t.Setenv(k, v)
	}
	code, out, errs := invoke(w.t, args)
	for k := range env {
		// Back to the world's value for the next step.
		switch k {
		case "ENVOY_FAKE_SCENARIO":
			w.t.Setenv(k, "success")
		default:
			w.t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	fmt.Fprintf(b, "$ envoy %s\nexit %d\n--- stdout\n%s--- stderr\n%s\n", w.quote(args), code, out, errs)
	return code
}

// quote prints argv with the world's paths written relative to $HOME, so the
// transcript names what the caller typed.
func (w *world) quote(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t") {
			a = "'" + a + "'"
		}
		q[i] = a
	}
	return strings.Join(q, " ")
}

var volatile = []struct {
	re   *regexp.Regexp
	with string
}{
	// A project's store directory is named for the checkout's path, which is
	// a new temporary directory on every run.
	{regexp.MustCompile(`repo-[0-9a-f]{8}`), "repo-<key>"},
	{regexp.MustCompile(`kill -INT \d+`), "kill -INT <pid>"},
	{regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:?\d\d)`), "<time>"},
	{regexp.MustCompile(`\b(pid|PID) \d+`), "$1 <pid>"},
}

func (w *world) normalise(s string) string {
	for _, home := range []string{w.home, realPath(w.home)} {
		s = strings.ReplaceAll(s, home, "$HOME")
	}
	if w.base != "" {
		s = strings.ReplaceAll(s, w.base, "<base>")
		s = strings.ReplaceAll(s, w.base[:7], "<base7>")
	}
	for _, v := range volatile {
		s = v.re.ReplaceAllString(s, v.with)
	}
	return s
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// pin compares a scenario's transcript with its file, or rewrites the file
// under ENVOY_CONTRACT_UPDATE=1.
func pin(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join(pkgDir, "testdata", "contract", name+".txt")
	if os.Getenv("ENVOY_CONTRACT_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%s: %v (record it with ENVOY_CONTRACT_UPDATE=1)", file, err)
	}
	if got != string(want) {
		t.Errorf("%s differs from what callers read:\n%s", file, lineDiff(string(want), got))
	}
}

// lineDiff names the first lines that differ, enough to find the change.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < max(len(w), len(g)) && shown < 12; i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			fmt.Fprintf(&b, "line %d\n  want: %s\n  got:  %s\n", i+1, wl, gl)
			shown++
		}
	}
	return b.String()
}

func TestContracts(t *testing.T) {
	scenarios := []struct {
		name string
		play func(w *world, b *strings.Builder)
	}{
		{"version", func(w *world, b *strings.Builder) {
			w.step(b, nil, "version")
			w.step(b, nil, "--version")
			w.step(b, nil, "-v")
		}},
		// Refusals: every one exits 3 and leaves no job directory.
		{"refusals", func(w *world, b *strings.Builder) {
			brief := w.path("brief.md")
			for _, args := range [][]string{
				{},
				{"nonsense"},
				{"run", "--prompt-file", brief, "--with", "codex"},
				{"run", "bad name", "--prompt-file", brief, "--with", "codex"},
				{"run", "job", "--prompt-file", brief},
				{"run", "job", "--with", "gemini", "--prompt-file", brief},
				{"run", "job", "--with", "codex"},
				{"run", "job", "--with", "codex=", "--prompt-file", brief},
				{"run", "job", "--with", "codex=" + w.path("absent.md")},
				{"run", "job", "--with", "codex", "--prompt-file", w.home},
				{"run", "job", "--with", "claude::minimal", "--prompt-file", brief},
				{"run", "job", "--with", "claude:opus:high:extra", "--prompt-file", brief},
				{"run", "job", "--with", "codex", "--with", "claude", "--prompt-file", brief, "--allow-write"},
				{"run", "job", "--with", "codex", "--prompt-file", brief, "extra"},
				{"run", "job", "--with", "codex", "--prompt-file", brief, "--model", "opus"},
				{"run", "job", "--with", "codex", "--prompt-file", brief, "--timeout-min", "-1"},
				{"run", "job", "--with", "codex", "--prompt-file", brief, "--timeout-min=-1"},
				{"run", "job", "--with", "codex", "--prompt-file", brief, "--max-budget-usd", "1"},
				{"run", "job", "--with", "claude", "--with", "codex", "--prompt-file", brief, "--max-budget-usd", "1"},
				{"run", "job", "--with", "claude", "--prompt-file", brief, "--version"},
				{"run", "job", "--with", "codex", "--prompt-file", brief, "--instructions-file", w.path("absent.md")},
				// A later seat's mistake, or one member's prompt that cannot
				// be read from its own seat or the default, refuses its
				// siblings too.
				{"run", "job", "--with", "codex=" + brief, "--with", "claude:opus"},
				{"run", "job", "--with", "codex=" + brief, "--with", "claude:opus=" + w.path("absent.md")},
				{"run", "job", "--with", "codex", "--with", "claude:opus=" + brief, "--prompt-file", w.path("absent.md")},
				{"collect"},
				{"collect", "--result-only", "--status-only", "job"},
				{"collect", "never-ran"},
				{"collect", "a", "b"},
				{"wait"},
				{"wait", "never-ran"},
				{"pending", "job"},
			} {
				w.step(b, nil, args...)
			}
			if entries, _ := os.ReadDir(filepath.Join(w.home, ".local", "state", "envoy", "jobs")); len(entries) > 0 {
				store, _ := os.ReadDir(filepath.Join(w.home, ".local", "state", "envoy", "jobs", entries[0].Name()))
				for _, e := range store {
					w.t.Errorf("a refused dispatch left %s behind", e.Name())
				}
			}
		}},
		// The review skill: one cold voice anchored on a baseline, read
		// status-only while uncollected, collected, waited for, listed.
		{"review", func(w *world, b *strings.Builder) {
			w.step(b, nil, "run", "review-r1", "--with", "codex:gpt-6-astra:high", "--prompt-file", w.path("brief.md"), "--baseline", w.base, "--timeout-min", "60")
			w.step(b, nil, "collect", "--status-only", "review-r1")
			w.step(b, nil, "pending")
			w.step(b, nil, "wait", "review-r1")
			w.step(b, nil, "collect", "review-r1")
			w.step(b, nil, "collect", "--result-only", "review-r1")
			w.step(b, nil, "pending")
			w.step(b, nil, "pending", "--base", filepath.Join(w.home, ".local", "state", "envoy", "jobs"))
		}},
		// The consult skill's three shapes and its round 2, then the review
		// skill seating a consult member warm beside a cold voice.
		{"consult", func(w *world, b *strings.Builder) {
			w.step(b, nil, "run", "consult-r1", "--with", "codex:gpt-6-astra:high", "--with", "claude:claude-opus-5-5", "--prompt-file", w.path("brief.md"), "--timeout-min", "30")
			w.step(b, nil, "collect", "consult-r1")
			w.step(b, nil, "run", "consult-r2", "--with", "claude:claude-fable-5-1="+w.path("critique.md"), "--with", "codex:gpt-6-astra:high="+w.path("survey.md"), "--timeout-min", "30")
			w.step(b, nil, "collect", "consult-r2")
			w.step(b, nil, "run", "consult-r3", "--with", "@consult-r1="+w.path("round2.md"), "--timeout-min", "30")
			w.step(b, nil, "collect", "consult-r3")
			w.step(b, nil, "run", "review-r1", "--with", "@consult-r1/codex-gpt-6-astra", "--with", "claude:claude-opus-5-5", "--prompt-file", w.path("brief.md"), "--baseline", w.base, "--timeout-min", "45")
			w.step(b, nil, "collect", "review-r1")
			w.step(b, nil, "run", "review-r2", "--with", "@consult-r1/codex-gpt-6-astra="+w.path("full.md"), "--with", "claude:claude-opus-5-5="+w.path("goal.md"), "--baseline", w.base, "--timeout-min", "45")
			w.step(b, nil, "collect", "review-r2")
		}},
		// The delegate skill: a write turn on a baseline.
		{"delegate", func(w *world, b *strings.Builder) {
			w.step(b, nil, "run", "delegate-r1", "--with", "codex:gpt-6.1-sol:xhigh", "--allow-write", "--baseline", w.base, "--prompt-file", w.path("brief.md"), "--timeout-min", "180")
			w.step(b, nil, "collect", "delegate-r1")
		}},
		// The planlab steward: job directories by path, a capped claude turn
		// with standing instructions, its continuation, a second opinion, and
		// a continuation refused because another live turn holds the session.
		{"steward", func(w *world, b *strings.Builder) {
			first, second := w.path("steward/turn-1"), w.path("steward/turn-2")
			w.step(b, nil, "run", first, "--prompt-file", w.path("brief.md"), "--with", "claude:claude-opus-5-5:high", "--allow-write", "--cwd", w.repo, "--timeout-min", "30", "--max-budget-usd", "0.50", "--instructions-file", w.path("rules.md"))
			w.step(b, nil, "collect", first)
			w.step(b, nil, "run", second, "--prompt-file", w.path("next.md"), "--with", "@"+first, "--allow-write", "--cwd", w.repo, "--timeout-min", "30")
			w.step(b, nil, "collect", second)
			w.step(b, nil, "run", w.path("consult/job"), "--prompt-file", w.path("brief.md"), "--with", "codex:gpt-6-astra:high", "--cwd", w.repo, "--timeout-min", "12")
			w.step(b, nil, "collect", w.path("consult/job"))
			locks := filepath.Join(w.home, ".local", "state", "envoy", "locks")
			if err := os.MkdirAll(locks, 0o755); err != nil {
				w.t.Fatal(err)
			}
			w.write(".local/state/envoy/locks/sess-claude.lock", fmt.Sprintf(`{"pid":%d,"runnerInstanceId":"other","outDir":"/tmp/live-job","startedAt":"2026-07-25T00:00:00.000Z"}`, os.Getpid()))
			w.step(b, nil, "run", w.path("steward/turn-3"), "--prompt-file", w.path("next.md"), "--with", "@"+second, "--allow-write", "--cwd", w.repo, "--timeout-min", "30")
		}},
		// Exit 1 and exit 6: a failed turn and a fan-out where one member
		// failed, each collected.
		{"failures", func(w *world, b *strings.Builder) {
			w.step(b, map[string]string{"ENVOY_FAKE_SCENARIO": "partial-failure"}, "run", "failed-r1", "--with", "claude", "--prompt-file", w.path("brief.md"))
			w.step(b, nil, "collect", "failed-r1")
			w.step(b, map[string]string{"ENVOY_FAKE_SCENARIO_CLAUDE": "partial-failure"}, "run", "partial-r1", "--with", "codex", "--with", "claude", "--prompt-file", w.path("brief.md"))
			w.step(b, nil, "collect", "partial-r1")
		}},
		// A spend cap that is not a number: 0.14 read NaN as its own "no cap"
		// sentinel and dispatched uncapped.
		{"budget-nan", func(w *world, b *strings.Builder) {
			w.step(b, nil, "run", "nan-r1", "--with", "claude", "--prompt-file", w.path("brief.md"), "--max-budget-usd", "NaN")
		}},
		// Long options written with one dash, which the standard library's
		// parser accepted.
		{"single-dash", func(w *world, b *strings.Builder) {
			w.step(b, nil, "run", "dash-r1", "-with", "codex", "-prompt-file", w.path("brief.md"))
		}},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			w := newWorld(t)
			var b strings.Builder
			s.play(w, &b)
			pin(t, s.name, w.normalise(b.String()))
		})
	}
}

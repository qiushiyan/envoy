// Package fan dispatches several provider turns at once and supervises them
// as a single job: one process to wait on, one directory to collect, one exit
// code. Each turn carries its own prompt — the caller resolved which file
// every member gets — so the group has no prompt of its own to record.
//
// A member is an ordinary turn — its own session, lock, job dir, prompt state
// and result.md — so nothing about the single-turn lifecycle lives here. This
// package only starts several runs, keeps their output from interleaving, and
// reports them as a set. Each run keeps its own event loop and its own process
// group; the runner is already parameterized by its writers and its out-dir,
// which is what makes several of them in one process safe.
package fan

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
	"github.com/qiushiyan/envoy/internal/runner"
	"github.com/qiushiyan/envoy/internal/text"
)

// Turn is one member: its address inside the fan-out and the turn it runs.
// The caller resolves both — the fan-out only supplies each turn's directory
// and writers, which are the two things a member cannot know about itself.
type Turn struct {
	Name    string
	Options runner.Options
}

// Options is one validated fan-out request: the turns, the directory that
// holds them, and the writers. The tree, anchor and cap are read from the
// turns, which the caller validated to share them — the manifest and the
// dispatch block describe the settings the members actually run with, never
// a second copy. The prompt is per turn and is not shared.
type Options struct {
	Turns  []Turn
	OutDir string // the fan-out directory, already reserved by the caller
	Stdout io.Writer
	Stderr io.Writer
}

// shared is the settings every member runs with, read from the first turn.
func (o Options) shared() runner.Options { return o.Turns[0].Options }

// outcome is one member's terminal state as the group reports it.
type outcome struct {
	name     string
	outDir   string
	status   string // "" = the turn never reached a terminal status
	exitCode int
}

// Run dispatches every member, waits for all of them, and returns the fan-out's
// exit code.
func Run(opts Options) int {
	startedAt := time.Now().Round(0)
	gw := job.GroupWorkspace{Dir: opts.OutDir}

	shared := opts.shared()
	// Member dirs exist before the manifest names them.
	names := make([]string, len(opts.Turns))
	for i, t := range opts.Turns {
		names[i] = t.Name
		if err := os.MkdirAll(gw.Member(t.Name).Dir, 0o755); err != nil {
			fmt.Fprintf(opts.Stderr, "envoy: cannot create member dir: %s\n", err)
			return job.ExitInfra
		}
	}
	group := &job.Group{
		SchemaVersion: job.GroupSchemaVersion,
		StartedAt:     job.ISO(startedAt),
		Cwd:           shared.Cwd,
		TimeoutMin:    shared.TimeoutMin,
		GitBaseline:   ptrIfNonEmpty(shared.Baseline),
		Members:       names,
	}
	// The manifest is the record that makes the directory a fan-out: discovery
	// keys on it, and the reserved name is released when it is absent. So a
	// manifest that cannot be written stops the dispatch before any member
	// starts, rather than running turns that no record would ever name.
	if err := group.WriteFile(gw.GroupPath()); err != nil {
		fmt.Fprintf(opts.Stderr, "envoy: cannot write group.json: %s\n", err)
		return job.ExitInfra
	}
	printDispatchBlock(opts, gw)

	// One signal channel for the fan-out as a whole. Each member's own event
	// loop receives the signal too and stops its own provider tree, so nothing
	// is fanned out here; this registration only keeps the process from taking
	// the default die-immediately path while members are still shutting down,
	// and reports the stop once instead of once per member.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case sig := <-sigCh:
			fmt.Fprintf(opts.Stdout, "\n%s\n", prose.FanStopping(sigName(sig), len(opts.Turns)))
		case <-done:
		}
	}()

	var mu sync.Mutex
	outcomes := make([]outcome, len(opts.Turns))
	var wg sync.WaitGroup
	for i, t := range opts.Turns {
		wg.Add(1)
		go func(i int, t Turn) {
			defer wg.Done()
			outcomes[i] = runMember(opts, t, gw.Member(t.Name).Dir, &mu)
		}(i, t)
	}
	wg.Wait()

	codes := make([]int, len(outcomes))
	statuses := make([]string, len(outcomes))
	for i, o := range outcomes {
		codes[i], statuses[i] = o.exitCode, o.status
	}
	printTerminalBlock(opts, outcomes, statuses)
	return job.ExitCodeForGroup(codes)
}

// runMember runs one turn to terminal state. Its stdout is dropped: every
// coordinate the single-turn block prints is also in the member's meta.json,
// and N interleaved blocks on one stdout would be unreadable for the caller
// the blocks are written for. Warnings on stderr are rare and load-bearing, so
// those are labelled and passed through.
//
// A panic is contained here rather than taking the sibling members down with
// the process. The member is then reported as one that published no status,
// and its provider — if it had started one — is left exactly as a killed
// runner leaves it, which is the case collect's recovery path already covers.
func runMember(opts Options, t Turn, outDir string, mu *sync.Mutex) (res outcome) {
	res = outcome{name: t.Name, outDir: outDir, exitCode: job.ExitInfra}
	stderr := &prefixWriter{mu: mu, w: opts.Stderr, prefix: t.Name}
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(stderr, "envoy: this member panicked and published no status: %v\n", p)
			res.exitCode, res.status = job.ExitInfra, ""
		}
		stderr.flush()
	}()
	turn := t.Options
	turn.OutDir = outDir
	turn.Stdout = io.Discard
	turn.Stderr = stderr
	r := runner.Run(turn)
	res.exitCode, res.status = r.ExitCode, r.Status
	return res
}

func printDispatchBlock(opts Options, gw job.GroupWorkspace) {
	w := opts.Stdout
	shared := opts.shared()
	fmt.Fprintf(w, "job: %s\n", gw.Dir)
	fmt.Fprintf(w, "fan-out: %d turns · hard cap %s each\n", len(opts.Turns), text.HardCap(shared.TimeoutMin))
	// The member name already carries its provider and model, so the line adds
	// only what the name cannot: the resolved settings, where it writes, and
	// which conversation it continues.
	for _, t := range opts.Turns {
		fmt.Fprintf(w, "member %s: model %s · effort %s · dir %s",
			t.Name, display(t.Options.Turn.Model), display(t.Options.Turn.Effort), gw.Member(t.Name).Dir)
		if t.Options.ResumedFrom != "" {
			fmt.Fprintf(w, " · continues %s", t.Options.ResumedFrom)
		}
		fmt.Fprintln(w)
	}
	if shared.Baseline != "" {
		fmt.Fprintf(w, "baseline: %s\n", shared.Baseline)
	}
	fmt.Fprintf(w, "next: %s\n", prose.FanDispatchNext(gw.Dir))
}

func printTerminalBlock(opts Options, outcomes []outcome, statuses []string) {
	w := opts.Stdout
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "status: %s\n", prose.FanStatusLine(statuses))
	for _, o := range outcomes {
		if o.status == "" {
			fmt.Fprintf(w, "member %s: %s\n", o.name, prose.FanUndispatched())
			continue
		}
		fmt.Fprintf(w, "member %s: %s · result %s\n", o.name, prose.StatusLine(o.status),
			job.Workspace{Dir: o.outDir}.ResultPath())
	}
	fmt.Fprintf(w, "group: %s\n", job.GroupWorkspace{Dir: opts.OutDir}.GroupPath())
	fmt.Fprintf(w, "next: %s\n", prose.FanNext(opts.OutDir, statuses))
}

// memberNameUnsafe is the member-name rule: lowercase, path-safe, and free of
// leading or trailing separators, because the name is both a directory and the
// label every group line uses for that member.
var memberNameUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// Name allocates a member's address from its base — the provider plus the
// model when one was named, or a preset name carried over from an earlier
// round — against the addresses already taken. A repeat is numbered, and the
// numbered form is checked against the taken set too: a base that happens to
// spell a sibling's numbered name must not land on that sibling's directory.
func Name(base string, taken map[string]bool) string {
	base = strings.Trim(memberNameUnsafe.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if base == "" {
		base = "member"
	}
	if len(base) > 40 {
		base = base[:40]
	}
	name := base
	for n := 2; taken[name]; n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	taken[name] = true
	return name
}

// prefixWriter labels one member's warnings on the shared stderr and
// serializes concurrent members, so a warning stays readable and attributable
// to the member that produced it.
type prefixWriter struct {
	mu     *sync.Mutex
	w      io.Writer
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		fmt.Fprintf(p.w, "[%s] %s\n", p.prefix, p.buf[:i])
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

// flush emits an unterminated tail, so a warning without a newline is not lost.
func (p *prefixWriter) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) > 0 {
		fmt.Fprintf(p.w, "[%s] %s\n", p.prefix, p.buf)
		p.buf = nil
	}
}

func display(v string) string {
	if v == "" {
		return "(provider default)"
	}
	return v
}

func ptrIfNonEmpty(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func sigName(sig os.Signal) string {
	if s, ok := sig.(syscall.Signal); ok {
		switch s {
		case syscall.SIGINT:
			return "SIGINT"
		case syscall.SIGTERM:
			return "SIGTERM"
		case syscall.SIGHUP:
			return "SIGHUP"
		}
	}
	return sig.String()
}

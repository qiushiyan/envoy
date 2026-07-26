// Package fan dispatches one prompt to several provider turns at once and
// supervises them as a single job: one process to wait on, one directory to
// collect, one exit code.
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
	"github.com/qiushiyan/envoy/internal/provider"
	"github.com/qiushiyan/envoy/internal/runner"
	"github.com/qiushiyan/envoy/internal/steer"
	"github.com/qiushiyan/envoy/internal/text"
)

// Member is one turn of a fan-out. Its name is derived from the provider and
// model, never supplied by the caller, so the directory layout and every line
// that mentions the member agree by construction.
type Member struct {
	Provider string
	Model    string // "" = the provider's own configured default
	Effort   string // "" = the provider's own configured default
}

// Options is one validated fan-out request. Everything here is shared by every
// member except the members themselves: the whole point is one prompt, one
// cap, one directory.
type Options struct {
	Members    []Member
	PromptFile string
	Cwd        string
	Baseline   string
	Label      string
	OutDir     string // "" = derive from cwd/label
	TimeoutMin float64
	Stdout     io.Writer
	Stderr     io.Writer
}

// outcome is one member's terminal state as the group reports it.
type outcome struct {
	name     string
	provider string
	outDir   string
	status   string // "" = the turn never reached a terminal status
	exitCode int
}

// Run dispatches every member, waits for all of them, and returns the fan-out's
// exit code.
func Run(opts Options) int {
	startedAt := time.Now().Round(0)
	names := memberNames(opts.Members)

	dir, err := job.ResolveOutDir(opts.OutDir, opts.Cwd, opts.Label, "fan", startedAt)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "envoy: cannot create out-dir: %s\n", err)
		return job.ExitInfra
	}
	gw := job.GroupWorkspace{Dir: dir}
	if err := gw.Prepare(opts.PromptFile); err != nil {
		fmt.Fprintf(opts.Stderr, "envoy: cannot prepare fan-out dir: %s\n", err)
		return job.ExitInfra
	}
	// Member dirs exist before the coordinates are published, so the watch
	// command works from the moment the caller reads it.
	members := make([]job.GroupMember, len(opts.Members))
	for i, m := range opts.Members {
		ws := gw.Member(names[i])
		if err := os.MkdirAll(ws.Dir, 0o755); err != nil {
			fmt.Fprintf(opts.Stderr, "envoy: cannot create member dir: %s\n", err)
			return job.ExitInfra
		}
		members[i] = job.GroupMember{
			Name:     names[i],
			Provider: m.Provider,
			Model:    ptrIfNonEmpty(m.Model),
			Effort:   ptrIfNonEmpty(m.Effort),
			OutDir:   ws.Dir,
		}
	}

	group := &job.Group{
		SchemaVersion: job.GroupSchemaVersion,
		StartedAt:     job.ISO(startedAt),
		Cwd:           opts.Cwd,
		PromptFile:    gw.PromptPath(),
		Label:         ptrIfNonEmpty(opts.Label),
		TimeoutMin:    opts.TimeoutMin,
		GitBaseline:   ptrIfNonEmpty(opts.Baseline),
		OutDir:        dir,
		WatchCommand:  gw.WatchCommand(),
		SupervisorPid: os.Getpid(),
		Members:       members,
	}
	writeGroup(group, gw, opts.Stderr)
	printDispatchBlock(opts, gw, members)

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
			fmt.Fprintf(opts.Stdout, "\n%s\n", steer.FanStopping(sigName(sig), len(members)))
		case <-done:
		}
	}()

	var mu sync.Mutex
	outcomes := make([]outcome, len(opts.Members))
	var wg sync.WaitGroup
	for i, m := range opts.Members {
		wg.Add(1)
		go func(i int, m Member) {
			defer wg.Done()
			outcomes[i] = runMember(opts, m, members[i], &mu)
		}(i, m)
	}
	wg.Wait()

	group.EndedAt = job.Ptr(job.ISO(time.Now()))
	writeGroup(group, gw, opts.Stderr)

	codes := make([]int, len(outcomes))
	statuses := make([]string, len(outcomes))
	for i, o := range outcomes {
		codes[i], statuses[i] = o.exitCode, o.status
	}
	printTerminalBlock(opts, dir, outcomes, statuses)
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
func runMember(opts Options, m Member, gm job.GroupMember, mu *sync.Mutex) (res outcome) {
	res = outcome{name: gm.Name, provider: m.Provider, outDir: gm.OutDir, exitCode: job.ExitInfra}
	stderr := &prefixWriter{mu: mu, w: opts.Stderr, prefix: gm.Name}
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(stderr, "envoy: this member panicked and published no status: %v\n", p)
			res.exitCode, res.status = job.ExitInfra, ""
		}
		stderr.flush()
	}()
	r := runner.Run(runner.Options{
		Provider:   m.Provider,
		PromptFile: opts.PromptFile,
		Cwd:        opts.Cwd,
		Baseline:   opts.Baseline,
		Label:      opts.Label,
		OutDir:     gm.OutDir,
		Turn: provider.Options{
			Model:      m.Model,
			Effort:     m.Effort,
			TimeoutMin: opts.TimeoutMin,
		},
		Stdout: io.Discard,
		Stderr: stderr,
	})
	res.exitCode, res.status = r.ExitCode, r.Status
	return res
}

func printDispatchBlock(opts Options, gw job.GroupWorkspace, members []job.GroupMember) {
	w := opts.Stdout
	fmt.Fprintf(w, "out-dir: %s\n", gw.Dir)
	fmt.Fprintf(w, "fan-out: %d turns · one prompt · hard cap %s each\n", len(members), text.HardCap(opts.TimeoutMin))
	// The member name already carries its provider and model, so the line adds
	// only what the name cannot: the resolved settings and where it writes.
	for _, m := range members {
		fmt.Fprintf(w, "member %s: model %s · effort %s · out-dir %s\n",
			m.Name, display(m.Model), display(m.Effort), m.OutDir)
	}
	if opts.Baseline != "" {
		fmt.Fprintf(w, "baseline: %s\n", opts.Baseline)
	}
	fmt.Fprintf(w, "watch: %s\n", gw.WatchCommand())
	fmt.Fprintf(w, "next: %s\n", steer.FanDispatchNext(gw.Dir))
}

func printTerminalBlock(opts Options, dir string, outcomes []outcome, statuses []string) {
	w := opts.Stdout
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "status: %s\n", steer.FanStatusLine(statuses))
	for _, o := range outcomes {
		if o.status == "" {
			fmt.Fprintf(w, "member %s: %s\n", o.name, steer.FanUndispatched())
			continue
		}
		fmt.Fprintf(w, "member %s: %s · result %s\n", o.name, steer.StatusLine(o.status),
			job.Workspace{Dir: o.outDir}.ResultPath())
	}
	fmt.Fprintf(w, "group: %s\n", job.GroupWorkspace{Dir: dir}.GroupPath())
	fmt.Fprintf(w, "next: %s\n", steer.FanNext(dir, statuses))
}

func writeGroup(group *job.Group, gw job.GroupWorkspace, stderr io.Writer) {
	if err := group.WriteFile(gw.GroupPath()); err != nil {
		fmt.Fprintf(stderr, "group write warning: %s\n", err)
	}
}

// memberNameUnsafe is the member-name rule: lowercase, path-safe, and free of
// leading or trailing separators, because the name is both a directory and the
// label every group line uses for that member.
var memberNameUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// memberNames labels each member with its provider, plus the model when one was
// named, so two members of the same family are distinguishable at a glance. A
// repeated pair — the same model dispatched twice on purpose — is numbered.
func memberNames(members []Member) []string {
	names := make([]string, len(members))
	seen := map[string]int{}
	for i, m := range members {
		base := m.Provider
		if m.Model != "" {
			base += "-" + m.Model
		}
		base = strings.Trim(memberNameUnsafe.ReplaceAllString(strings.ToLower(base), "-"), "-")
		if base == "" {
			base = "member"
		}
		if len(base) > 40 {
			base = base[:40]
		}
		seen[base]++
		if n := seen[base]; n > 1 {
			names[i] = fmt.Sprintf("%s-%d", base, n)
		} else {
			names[i] = base
		}
	}
	return names
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

func display(v *string) string {
	if v == nil {
		return "(provider default)"
	}
	return *v
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

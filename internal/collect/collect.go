// Package collect recovers or prints one finished (or stranded) job as a
// single scannable block, and discovers pending jobs after a possibly missed
// completion notification. Explicit collection stamps terminal jobs with
// collectedAt once their block has reached the caller; pending discovery
// never marks anything collected.
//
// Every command and prescription a caller reads is rendered here, from the
// job's records and the directory they were read from. The runner records
// facts; this package words them.
package collect

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/qiushiyan/envoy/internal/gitx"
	"github.com/qiushiyan/envoy/internal/job"
)

// Mode selects which sections of a job's block collection prints. The default
// prints everything; the narrowed modes exist because the block is read by an
// agent whose context the result body and the status preamble compete for.
type Mode int

const (
	// ModeFull prints the whole block and stamps terminal collection.
	ModeFull Mode = iota
	// ModeResultOnly prints an ok turn's result body alone and stamps it
	// collected. A turn that is not ok prints its full block instead: its
	// status and next action are its result, and suppressing them would hand
	// the caller a payload that does not exist.
	ModeResultOnly
	// ModeStatusOnly prints everything except the result body and stamps
	// nothing: the result was not delivered, so the job stays uncollected.
	// It is also the full-preamble read: a healthy turn's own block holds
	// back the diagnostic fields, and this is the flag that asks for them
	// without a status having to go wrong first.
	ModeStatusOnly
)

// Collect prints the job ref names — a name in this project's store, or a
// directory — whether a single turn, or a whole fan-out with a section per
// member — and stamps first terminal collection once the block has reached
// the caller. code is the process exit code; a ref that names no job comes
// back as err, unprinted, and everything else is reported here.
//
// Reading, writing and acknowledgment are separate steps: everything the
// block depends on is read first, the block is rendered in full and written to
// the caller second, and only a write that succeeded stamps anything. A caller
// whose output broke mid-block is still owed the job, and pending discovery
// keeps listing it.
func Collect(ref string, mode Mode, w, errW io.Writer) (code int, err error) {
	dir, note, code, err := resolve(ref)
	if err != nil {
		return code, err
	}
	return collectDir(dir, note, mode, w, errW), nil
}

// collectDir prints the job in dir, under the note its name resolved with.
func collectDir(dir, note string, mode Mode, w, errW io.Writer) int {
	if job.IsGroupDir(dir) {
		return collectGroup(dir, note, mode, w, errW)
	}
	meta, err := job.ReadMeta(dir)
	if err != nil {
		reportUnreadable(dir, err, errW)
		return job.ExitUsage
	}
	t := readTurn(dir, meta)
	var body bytes.Buffer
	t.write(&body, mode, false)
	if !deliver(w, errW, withNote(body.Bytes(), note, mode, errW)) {
		return job.ExitInfra
	}
	if t.stamps(mode) {
		stampCollected(dir, t.meta)
	}
	return 0
}

// withNote places what the caller must know about how its reference was
// resolved directly under the block's first line, which names the job it
// resolved to. A result-only read is the payload alone, so there the note
// goes to the error stream instead of into the result.
func withNote(body []byte, note string, mode Mode, errW io.Writer) []byte {
	if note == "" {
		return body
	}
	if mode == ModeResultOnly {
		fmt.Fprintf(errW, "note: %s\n", note)
		return body
	}
	head, rest, _ := bytes.Cut(body, []byte("\n"))
	return slices.Concat(head, []byte("\nnote: "+note+"\n"), rest)
}

// deliver writes a rendered block to the caller and reports whether all of
// it arrived. A short or failed write is reported on stderr so the caller
// knows the job is still owed.
func deliver(w, errW io.Writer, body []byte) bool {
	n, err := w.Write(body)
	if err == nil && n < len(body) {
		err = io.ErrShortWrite
	}
	if err != nil {
		fmt.Fprintf(errW, "collect error: the block was not delivered (%s), so nothing was marked collected; collect again once the output is readable\n", err)
		return false
	}
	return true
}

// reportUnreadable says on the error stream why a turn's record could not be
// read, and where the evidence of what it did still is.
func reportUnreadable(dir string, err error, errW io.Writer) {
	ws := job.Workspace{Dir: dir}
	if errors.Is(err, job.ErrNoRecord) {
		fmt.Fprintf(errW,
			"collect error: %s not found — inspect %s, %s, %s, and the working tree before deciding whether to retry\n",
			ws.MetaPath(), ws.ProgressLogPath(), ws.RawLogPath(), ws.StderrLogPath())
		return
	}
	fmt.Fprintf(errW,
		"collect error: %s could not be read (%s). Inspect %s, %s, %s, and the working tree; do not assume the job finished or retry it blindly.\n",
		ws.MetaPath(), err, ws.ProgressLogPath(), ws.RawLogPath(), ws.StderrLogPath())
}

// reportUnreadableManifest says on the error stream that a fan-out's manifest
// could not be read, by the verb that tried, and that each member can still be
// read on its own.
func reportUnreadableManifest(verb, dir string, err error, errW io.Writer) {
	fmt.Fprintf(errW, "%s error: %s could not be read (%s). Each member's job dir under %s is self-contained — collect one directly to see its result.\n",
		verb, job.GroupWorkspace{Dir: dir}.GroupPath(), err, dir)
}

// stampCollected marks first terminal collection: the block has actually
// been delivered to a caller, so pending discovery stops listing the job.
func stampCollected(dir string, meta *job.Meta) {
	if meta.CollectedAt != nil {
		return
	}
	meta.CollectedAt = new(job.ISO(time.Now()))
	meta.WriteFile(job.Workspace{Dir: dir}.MetaPath())
}

// printGitSinceBaseline shows what changed in the tree since a turn's anchor:
// the commits, the diffstat, and whether anything is still uncommitted.
func printGitSinceBaseline(w io.Writer, cwd, baseline string) {
	fmt.Fprintf(w, "\n--- git since baseline %s (in %s) ---\n", baseline, cwd)
	fmt.Fprintln(w, orDefault(gitx.Run(cwd, "log", baseline+"..HEAD", "--oneline"), "(no commits)"))
	fmt.Fprintln(w, orDefault(gitx.Run(cwd, "diff", baseline, "--stat"), "(no diff)"))
	if dirty := gitx.Run(cwd, "status", "--short"); dirty != "" {
		fmt.Fprintf(w, "dirty:\n%s\n", dirty)
	} else {
		fmt.Fprintln(w, "tree clean")
	}
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

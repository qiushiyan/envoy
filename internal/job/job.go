// Package job owns the durable artifact set of one turn: the out-dir layout,
// the meta.json schema, and the progress log. turn and collect both go through
// this package, so the writer and the reader of a job cannot drift apart.
package job

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/qiushiyan/envoy/internal/gitx"
)

// Exit codes are part of the caller interface; see ENGINE contract.
const (
	ExitOK          = 0
	ExitFailed      = 1
	ExitInfra       = 2
	ExitUsage       = 3
	ExitTimeout     = 4
	ExitInterrupted = 5
)

// Turn statuses that can appear in meta.json.
const (
	StatusRunning     = "running"
	StatusOK          = "ok"
	StatusFailed      = "failed"
	StatusInfra       = "infra"
	StatusTimeout     = "timeout"
	StatusInterrupted = "interrupted"
	StatusAbandoned   = "abandoned" // written by collect's reconciliation, never by the runner
)

// KnownStatus reports whether s is a status this engine writes — the
// discriminator between a job's meta.json and some other program's file that
// happens to parse and carry a "status" field.
func KnownStatus(s string) bool {
	switch s {
	case StatusRunning, StatusOK, StatusFailed, StatusInfra, StatusTimeout, StatusInterrupted, StatusAbandoned:
		return true
	}
	return false
}

// ExitCodeFor maps a terminal status to the process exit code.
func ExitCodeFor(status string) int {
	switch status {
	case StatusOK:
		return ExitOK
	case StatusFailed:
		return ExitFailed
	case StatusTimeout:
		return ExitTimeout
	case StatusInterrupted:
		return ExitInterrupted
	default:
		return ExitInfra
	}
}

// Prompt states; "accepted" is the load-bearing one for recovery decisions.
const (
	PromptUnknown    = "unknown"
	PromptAccepted   = "accepted"
	PromptNotStarted = "not_started"
)

// Result kinds recorded in meta.json.
const (
	ResultNone    = "none"
	ResultPartial = "partial"
	ResultFinal   = "final"
)

// ISO renders timestamps exactly as the meta schema and progress log expect:
// UTC with millisecond precision and a trailing Z.
func ISO(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// RandomHex returns n bytes of entropy as lowercase hex.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure means the platform is broken
	}
	return hex.EncodeToString(b)
}

// UUID4 returns a random RFC 9562 version-4 UUID string.
func UUID4() string { return uuid.NewV4().String() }

// WriteFileAtomic replaces path by writing a sibling temp file and renaming it,
// so a kill can never leave a half-written document.
func WriteFileAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.%d.%s.tmp", path, os.Getpid(), RandomHex(8))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// StateDir is the engine's per-user state root (locks, out-of-repo job dirs).
func StateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".local", "state", "envoy")
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// ProjectSlug names a project's slice of the central job store: the anchor's
// basename for a human, a short hash of its real path for uniqueness. The
// anchor is the git root when cwd is inside a repo — a turn dispatched from a
// subdirectory belongs to the same project.
func ProjectSlug(cwd string) string {
	anchor := gitx.Root(cwd)
	if anchor == "" {
		anchor = cwd
	}
	// Normalize symlinked spellings (/tmp vs /private/tmp) so the slug does
	// not depend on how the caller spelled the path.
	if real, err := filepath.EvalSymlinks(anchor); err == nil {
		anchor = real
	}
	base := slugUnsafe.ReplaceAllString(strings.ToLower(filepath.Base(anchor)), "-")
	if base == "" || base == "-" {
		base = "root"
	}
	if len(base) > 40 {
		base = base[:40]
	}
	sum := sha256.Sum256([]byte(anchor))
	return base + "-" + hex.EncodeToString(sum[:4])
}

// DefaultBase is where a project's named jobs live: central, never inside
// the project tree. A bare job name resolves under it (see ResolveRef).
func DefaultBase(cwd string) string {
	return filepath.Join(StateDir(), "jobs", ProjectSlug(cwd))
}

// Workspace is one job directory and the fixed names inside it.
type Workspace struct {
	Dir string
}

func (w Workspace) PromptPath() string      { return filepath.Join(w.Dir, "prompt.md") }
func (w Workspace) ResultPath() string      { return filepath.Join(w.Dir, "result.md") }
func (w Workspace) MetaPath() string        { return filepath.Join(w.Dir, "meta.json") }
func (w Workspace) RawLogPath() string      { return filepath.Join(w.Dir, "raw.log") }
func (w Workspace) StderrLogPath() string   { return filepath.Join(w.Dir, "stderr.log") }
func (w Workspace) ProgressLogPath() string { return filepath.Join(w.Dir, "progress.log") }
func (w Workspace) LastMessagePath() string { return filepath.Join(w.Dir, "last-message.txt") }

// Prepare archives the prompt exactly as it will be sent and truncates the
// log files, so a crashed job still leaves a complete, self-describing
// directory. It takes the bytes, not the file: the runner reads the caller's
// file once and both sends and archives that one read, so prompt.md cannot
// differ from what the provider received by a write that landed in between.
func (w Workspace) Prepare(prompt []byte) error {
	if err := os.WriteFile(w.PromptPath(), prompt, 0o644); err != nil {
		return err
	}
	for _, p := range []string{w.RawLogPath(), w.StderrLogPath(), w.ProgressLogPath()} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			return err
		}
	}
	return nil
}

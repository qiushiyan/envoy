// Package job owns the durable artifact set of one turn: the out-dir layout,
// the meta.json schema, and the progress log. turn and collect both go through
// this package, so the writer and the reader of a job cannot drift apart.
package job

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/gitx"
	"github.com/qiushiyan/envoy/internal/text"
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

// UUID4 returns a random RFC 4122 version-4 UUID string.
func UUID4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

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

// DefaultBase is where jobs land when --out-dir is not given: a self-ignored
// .envoy/ dir at the repo root, or a per-directory dir under the state root.
func DefaultBase(cwd string) string {
	if root := gitx.Root(cwd); root != "" {
		return filepath.Join(root, ".envoy")
	}
	return filepath.Join(StateDir(), filepath.Base(cwd))
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// ResolveOutDir creates and returns the job directory for a new turn.
func ResolveOutDir(explicit, cwd, label, provider string, now time.Time) (string, error) {
	if explicit != "" {
		return explicit, os.MkdirAll(explicit, 0o755)
	}
	base := DefaultBase(cwd)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	if gitx.Root(cwd) != "" {
		// Self-ignore; never touch the repo's own .gitignore.
		ignore := filepath.Join(base, ".gitignore")
		if _, err := os.Stat(ignore); os.IsNotExist(err) {
			os.WriteFile(ignore, []byte("*\n"), 0o644)
		}
	}
	stamp := now.Format("20060102-150405")
	slugSrc := label
	if slugSrc == "" {
		slugSrc = provider
	}
	slug := slugUnsafe.ReplaceAllString(strings.ToLower(slugSrc), "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	dir := filepath.Join(base, stamp+"-"+slug)
	if _, err := os.Stat(dir); err == nil {
		dir = dir + "-" + RandomHex(2)
	}
	return dir, os.MkdirAll(dir, 0o755)
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

// WatchCommand is the optional live view offered to the caller. Observation
// only — never a completion signal.
func (w Workspace) WatchCommand() string {
	return fmt.Sprintf("tail -f %s %s", text.ShellQuote(w.ProgressLogPath()), text.ShellQuote(w.RawLogPath()))
}

// Prepare copies the dispatched prompt in and truncates the log files, so a
// crashed job still leaves a complete, self-describing directory.
func (w Workspace) Prepare(promptFile string) error {
	src, err := os.Open(promptFile)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(w.PromptPath())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	for _, p := range []string{w.RawLogPath(), w.StderrLogPath(), w.ProgressLogPath()} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			return err
		}
	}
	return nil
}

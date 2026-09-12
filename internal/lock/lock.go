// Package lock enforces one live turn per provider session. A concurrent
// second turn on a session — including a --resume racing a live one — corrupts
// the conversation, so the race must fail fast instead of silently
// interleaving.
//
// An existing lock is never reclaimed here: a dead runner can leave a live
// orphan provider, and automatic stale takeover cannot be made race-free with
// a plain lock file. Recovery must inspect first.
package lock

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/text"
)

type payload struct {
	Pid              int    `json:"pid"`
	RunnerInstanceID string `json:"runnerInstanceId"`
	OutDir           string `json:"outDir"`
	StartedAt        string `json:"startedAt"`
}

// Conflict reports that a session lock is held — by a provably live owner or
// by one that cannot be proven dead (never auto-reclaimed either way). The
// message is agent-facing prose and carries the distinction.
type Conflict struct {
	SessionID string
	Message   string
}

func (c *Conflict) Error() string { return c.Message }

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Path returns the lock file location for a session id.
func Path(sessionID string) string {
	return filepath.Join(job.StateDir(), "locks", unsafeChars.ReplaceAllString(sessionID, "-")+".lock")
}

// Handle is a held lock; Release removes it if still ours.
type Handle struct {
	path             string
	runnerInstanceID string
}

// Acquire takes the session lock or returns a *Conflict. Any other error is
// an infrastructure failure.
func Acquire(sessionID, outDir, runnerInstanceID string) (*Handle, error) {
	p := Path(sessionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	data, err := json.Marshal(payload{
		Pid:              os.Getpid(),
		RunnerInstanceID: runnerInstanceID,
		OutDir:           outDir,
		StartedAt:        job.ISO(time.Now()),
	})
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err == nil {
		_, werr := f.Write(append(data, '\n'))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(p)
			return nil, werr
		}
		return &Handle{path: p, runnerInstanceID: runnerInstanceID}, nil
	}
	if !os.IsExist(err) {
		return nil, err
	}

	var held *payload
	if raw, rerr := os.ReadFile(p); rerr == nil {
		var parsed payload
		if json.Unmarshal(raw, &parsed) == nil {
			held = &parsed
		}
	}
	if held != nil && proc.Alive(held.Pid) {
		return nil, &Conflict{
			SessionID: sessionID,
			Message: fmt.Sprintf(
				"session %s already has a live turn (pid %d, started %s, job %s). One turn per session: wait for that process to exit, then collect its job: envoy collect %s",
				sessionID, held.Pid, held.StartedAt, held.OutDir, text.ShellQuote(held.OutDir)),
		}
	}
	inspect := ""
	if held != nil && held.OutDir != "" {
		inspect = fmt.Sprintf("Run envoy collect %s and inspect its provider state. ", text.ShellQuote(held.OutDir))
	}
	return nil, &Conflict{
		SessionID: sessionID,
		Message: fmt.Sprintf(
			"session %s has an existing lock whose runner is not provably live (%s). Automatic takeover is refused because its provider may still be running. %sOnly after the provider is gone and its work is accounted for, remove the stale lock and retry.",
			sessionID, p, inspect),
	}
}

// Release removes the lock unless another runner acquired it after manual
// cleanup or PID reuse — only the instance that took it may drop it. The
// returned error is advisory: the turn is already terminal when this runs,
// so callers report it and move on.
func (h *Handle) Release() error {
	if h == nil {
		return nil
	}
	raw, err := os.ReadFile(h.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var held payload
	if json.Unmarshal(raw, &held) == nil && held.RunnerInstanceID != h.runnerInstanceID {
		return nil // a foreign lock is not ours to drop
	}
	if err := os.Remove(h.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

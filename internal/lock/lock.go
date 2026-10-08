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
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

type payload struct {
	Pid              int    `json:"pid"`
	RunnerInstanceID string `json:"runnerInstanceId"`
	OutDir           string `json:"outDir"`
	StartedAt        string `json:"startedAt"`
}

// Conflict reports that a session lock is held — by a provably live owner or
// by one that cannot be proven dead (never auto-reclaimed either way). It
// carries the observation; internal/prose words it.
type Conflict struct {
	job.LockConflict
}

func (c *Conflict) Error() string { return prose.SessionLock(&c.LockConflict) }

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// lockPath returns the lock file location for a session id.
func lockPath(sessionID string) string {
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
	p := lockPath(sessionID)
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
	conflict := &Conflict{job.LockConflict{SessionID: sessionID, LockPath: p}}
	if held != nil {
		conflict.Holder = &job.LockHolder{Pid: held.Pid, StartedAt: held.StartedAt, Dir: held.OutDir}
		// Only a provably live owner is waited on; every other probe answer is
		// refused as well, and reclaims nothing either. The owner is read the
		// way every runner is: by its claim on its job directory when its
		// record says it holds one, by its PID otherwise.
		claimed := false
		if stamp, err := job.ReadStamp(held.OutDir); err == nil {
			claimed = stamp.RunnerLock
		}
		conflict.HolderLive = proc.RunnerLiveness(held.OutDir, held.Pid, claimed) == proc.Live
	}
	return nil, conflict
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

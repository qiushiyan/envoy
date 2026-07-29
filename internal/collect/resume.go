package collect

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
)

// ResumeBlocker is one member standing between a fan-out and a set-level
// resume, classified in prose's vocabulary so every surface words it the
// same way.
type ResumeBlocker struct {
	Member string
	Kind   prose.ResumeBlockerKind
	Detail string // the read error for an unreadable meta; "" otherwise
}

// ResumableMember is one member of a finished fan-out with a session to
// continue, carrying the roster settings a resumed turn re-dispatches with
// and the job dir whose conversation it continues.
type ResumableMember struct {
	Name     string
	Provider string
	Model    string
	Effort   string
	Session  string
	OutDir   string
}

// FanResumeState is what a set-level resume decision needs: the manifest,
// the members that can continue, and the members that block the round.
type FanResumeState struct {
	Group    *job.Group
	Members  []ResumableMember
	Blockers []ResumeBlocker
}

// memberResumeBlocker is the single definition of "this member blocks a
// set-level resume", shared by the dispatch-side inspection and collect's
// resume line so the two can never advertise different sets. A recorded
// session-lock conflict blocks the set exactly as it suppresses that
// member's own resume command: the session may belong to another job.
func memberResumeBlocker(meta *job.Meta) (prose.ResumeBlockerKind, bool) {
	switch {
	case meta == nil:
		return prose.BlockerUnreadableMeta, true
	case meta.Status == job.StatusRunning:
		return prose.BlockerRunning, true
	case meta.SessionLockConflict != nil:
		return prose.BlockerLockConflict, true
	case meta.SessionID == nil:
		return prose.BlockerNoSession, true
	}
	return "", false
}

// InspectFanResume reads a fan-out's manifest and each member's own meta.json
// and reports what a set-level resume may continue. The error is non-nil only
// when dir holds no readable fan-out at all; a blocked set is a valid state.
func InspectFanResume(dir string) (*FanResumeState, error) {
	group, err := job.ReadGroupFile(job.GroupWorkspace{Dir: dir}.GroupPath())
	if err != nil {
		return nil, err
	}
	state := &FanResumeState{Group: group}
	for _, m := range group.Members {
		meta, _, err := job.ReadMetaFile(filepath.Join(m.OutDir, "meta.json"))
		if err != nil {
			meta = nil
		}
		if kind, blocked := memberResumeBlocker(meta); blocked {
			detail := ""
			if err != nil {
				detail = err.Error()
			}
			state.Blockers = append(state.Blockers, ResumeBlocker{Member: m.Name, Kind: kind, Detail: detail})
			continue
		}
		state.Members = append(state.Members, ResumableMember{
			Name:     m.Name,
			Provider: m.Provider,
			Model:    deref(m.Model),
			Effort:   deref(m.Effort),
			Session:  *meta.SessionID,
			OutDir:   m.OutDir,
		})
	}
	return state, nil
}

// ResumableTurn is a finished turn whose session a follow-up may continue,
// carrying the recorded settings the follow-up inherits unless the caller
// overrides them.
type ResumableTurn struct {
	Provider   string
	Model      string
	Effort     string
	Cwd        string
	Baseline   string
	Session    string
	AllowWrite bool
}

// InspectTurnResume reads dir as a single turn and reports whether its
// session can be continued — the dispatch-side twin of collect's own resume
// line, sharing memberResumeBlocker so the two can never disagree about which
// jobs may continue. The error is non-nil only when dir holds no readable
// turn at all; a real turn that cannot continue reports a blocker instead.
func InspectTurnResume(dir string) (*ResumableTurn, prose.ResumeBlockerKind, error) {
	metaPath := filepath.Join(dir, "meta.json")
	if _, err := os.Stat(metaPath); err != nil {
		return nil, "", fmt.Errorf("no turn found there (no meta.json)")
	}
	meta, _, err := job.ReadMetaFile(metaPath)
	if err != nil {
		return nil, "", fmt.Errorf("meta.json is unreadable (%s)", err)
	}
	if meta.SchemaVersion < 1 || !job.KnownStatus(meta.Status) || meta.Provider == "" {
		return nil, "", fmt.Errorf("meta.json is not a job this engine wrote (status %q)", meta.Status)
	}
	if kind, blocked := memberResumeBlocker(meta); blocked {
		return nil, kind, nil
	}
	return &ResumableTurn{
		Provider:   meta.Provider,
		Model:      deref(meta.Model),
		Effort:     deref(meta.Effort),
		Cwd:        meta.Cwd,
		Baseline:   deref(meta.GitBaseline),
		Session:    *meta.SessionID,
		AllowWrite: meta.AllowWrite,
	}, "", nil
}

// TurnResume reports whether dir holds a single turn, and that turn's own
// resume command when it has one — the redirect a --resume-from aimed at a
// turn instead of a fan-out hands back.
func TurnResume(dir string) (cmd string, isTurn bool) {
	metaPath := filepath.Join(dir, "meta.json")
	if _, err := os.Stat(metaPath); err != nil {
		return "", false
	}
	meta, _, err := job.ReadMetaFile(metaPath)
	if err != nil {
		return "", true
	}
	return resumeCommand(meta), true
}

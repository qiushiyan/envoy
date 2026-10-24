package collect

import (
	"errors"
	"os"
	"slices"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// NameHold is the single definition of "the job in dir still holds its name
// against caller" ("" = a caller with no identity). A job holds while it is
// undelivered — some turn in it not yet terminal and collected, which is what
// collectedAt means — so a caller may still be waiting to collect it by that
// name, and only against a caller that shares its meaning of the name
// (job.SharesName): the same caller, or either side without an identity. Who
// dispatched the job is read from the same record that says whether it is
// delivered; a record that cannot say binds like one with no identity.
//
// Age is not consulted: a job nobody collected holds until someone does, and
// pending lists it. A directory with no record holds too: it is a dispatch
// between its reservation and its first record, or one that died there, and
// the engine cannot tell which.
//
// A fan-out holds through any member that does. A member the roster names
// that has no record — a session held at dispatch refuses that member alone,
// before it writes anything — has nothing a collect could ever deliver, but
// its absence alone does not say it was refused: members start independently,
// and one may still be preparing its turn. What says it never will start is
// the supervising process being gone, so a recordless member holds for as
// long as that process may be alive, and an older manifest that does not name
// it holds for good.
func NameHold(dir, caller string) (prose.NameHold, bool) {
	hold, owner, held := undelivered(dir)
	if !held || !job.SharesName(caller, owner) {
		return prose.NameHold{}, false
	}
	return hold, true
}

// undelivered reads whether the job in dir has anything left to deliver, and
// the caller its record names ("" for none, or a record that cannot say).
func undelivered(dir string) (hold prose.NameHold, owner string, held bool) {
	if !job.IsGroupDir(dir) {
		r := job.ReadRecord(dir)
		if errors.Is(r.Err, job.ErrNoRecord) {
			entries, err := os.ReadDir(dir)
			return prose.NameHold{Kind: prose.HoldUnrecorded, Empty: err == nil && len(entries) == 0}, "", true
		}
		if r.Stamp != nil {
			owner = job.Deref(r.Stamp.Caller)
		}
		hold, held = stampHold(r)
		return hold, owner, held
	}
	stamp, group, err := job.ReadGroupRecord(dir)
	if stamp != nil {
		owner = job.Deref(stamp.Caller)
	}
	var other *job.SchemaError
	switch {
	case errors.As(err, &other):
		hold, held = otherFanHold(dir, stamp)
		return hold, owner, held
	case err != nil:
		return prose.NameHold{Kind: prose.HoldUnreadable}, owner, true
	}
	mayStillStart := proc.RunnerLiveness(dir, group.RunnerPid, group.RunnerLock) != proc.Gone
	for _, m := range job.ReadMembers(dir, group) {
		if errors.Is(m.Err, job.ErrNoRecord) {
			if mayStillStart {
				return prose.NameHold{Kind: prose.HoldUnrecorded, Member: m.Name}, owner, true
			}
			continue
		}
		if hold, held := stampHold(m.Record); held {
			hold.Member = m.Name
			return hold, owner, true
		}
	}
	return prose.NameHold{}, owner, false
}

// otherFanHold is the hold another schema version's fan-out places on its
// name, decided from its stamp alone: like another version's turn, it holds
// only while its own engine may still be running it — the runner its stamp
// records, not provably gone. This engine can deliver nothing under that
// name, so a hold past the runner would strand it for good, and a manifest
// that records no runner gives this engine nothing to wait on.
func otherFanHold(dir string, stamp *job.GroupStamp) (prose.NameHold, bool) {
	if (stamp.RunnerPid > 0 || stamp.RunnerLock) && proc.RunnerLiveness(dir, stamp.RunnerPid, stamp.RunnerLock) != proc.Gone {
		return prose.NameHold{Kind: prose.HoldOtherVersion}, true
	}
	return prose.NameHold{}, false
}

// stampHold is the hold a turn record's stamp places on its name; a record
// with no stamp is one that does not parse, which holds as unreadable.
//
// A record of another schema version holds only while its own engine may
// still be running it. That turn is live work under the name until its runner
// is gone; after that this engine cannot deliver the record, so no caller can
// be waiting to collect it by the name here — and since no collect of this
// engine can reconcile it either, holding past the runner would strand the
// name for good.
func stampHold(r job.Record) (prose.NameHold, bool) {
	stamp := r.Stamp
	switch {
	case stamp == nil:
		return prose.NameHold{Kind: prose.HoldUnreadable}, true
	case !slices.Contains(job.MetaSchemas, stamp.SchemaVersion):
		if stamp.Status == job.StatusRunning && proc.RunnerLiveness(r.Dir, stamp.RunnerPid, stamp.RunnerLock) != proc.Gone {
			return prose.NameHold{Kind: prose.HoldOtherVersion}, true
		}
		return prose.NameHold{}, false
	case stamp.Status == job.StatusRunning:
		return prose.NameHold{Kind: prose.HoldRunning}, true
	case stamp.CollectedAt == nil:
		return prose.NameHold{Kind: prose.HoldUncollected}, true
	}
	return prose.NameHold{}, false
}

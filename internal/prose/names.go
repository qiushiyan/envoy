package prose

import (
	"fmt"
)

// UnreadableStore refuses to report an unreadable job root as an empty one.
// The two look identical from the outside and mean opposite things: a mistyped
// --base and a project that has never dispatched both print zero jobs, and
// only one of them is a fact the engine observed.
func UnreadableStore(base string, err error) string {
	return fmt.Sprintf("the job store at %s could not be read: %s. "+
		"This is not the same as an empty store, so nothing here says whether that project has jobs — "+
		"check the path and its permissions.", base, err)
}

// NameFellBack is the note on a block whose name did not resolve to a job of
// the caller's own: the caller has an identity and no job under the name, so
// it read the newest of anyone's. That is how earlier work is picked up, and
// also what a caller whose identity changed mid-task gets in place of its
// own job — the engine cannot tell the two apart, so it says which happened
// and leaves the judgment to the caller.
func NameFellBack(name string, ownerKnown bool) string {
	owner := "a caller with no recorded session dispatched it"
	if ownerKnown {
		owner = "another session dispatched it"
	}
	return fmt.Sprintf("this session has dispatched no job named %s, so this is the newest job under that name — %s. "+
		"Check it is the job you mean before using it: a job's prompt.md holds what it was asked.", name, owner)
}

// ContinuedAnotherSession is the note on a turn whose conversation was begun
// by a different session than the one that dispatched this turn.
func ContinuedAnotherSession() string {
	return "this turn continued a conversation another session began (resumed-from, above). " +
		"If you meant to continue your own, this result does not belong to it: send the follow-up again, naming your own job by its directory."
}

// UnattributedGeneration stops a name from resolving past a job whose record
// cannot say whose it is. Skipping it would let an older job of the caller's
// answer to the name as though it were the latest, which is the wrong-job
// read names exist to prevent.
func UnattributedGeneration(name, dir string, err error) string {
	return fmt.Sprintf("the name %s was not resolved: the record in %s could not be read (%s), so the name cannot be shown to mean this session's latest job. "+
		"Name the job you want by its directory; to see what that one holds: %s", name, dir, err, CollectCommand(dir))
}

// JobExists refuses a directory path that already exists. A path is an
// identity — one directory, one job — so unlike a name it has no generations.
func JobExists(dir string) string {
	return fmt.Sprintf("job directory %s already exists — a directory holds one job. Pick another path; "+
		"to read the existing job: %s", dir, CollectCommand(dir))
}

// NameHoldKind classifies why a job still holds its name. The vocabulary is
// owned here; internal/collect decides which kind applies.
type NameHoldKind string

const (
	HoldRunning     NameHoldKind = "running"
	HoldUncollected NameHoldKind = "uncollected"
	HoldUnrecorded  NameHoldKind = "unrecorded"
	HoldUnreadable  NameHoldKind = "unreadable"
)

// NameHold is what internal/collect observed holding a name: why, through
// which fan-out member if any, and — for a directory with no record —
// whether it was seen to be empty.
type NameHold struct {
	Kind   NameHoldKind
	Member string
	Empty  bool
}

// NameHeld refuses a dispatch under a name held by an undelivered job that
// shares the dispatcher's meaning of it. Every other reuse of a name is
// silent, so this is the one collision a caller ever hears about, and it leads with the fact a caller must not
// miss: nothing ran, so collecting this name reads the job already there —
// the caller's own earlier one, or another caller's. dir is the job — for a
// fan-out the whole one, since one collect covers every member.
func NameHeld(name, dir string, hold NameHold) string {
	subject := "which"
	if hold.Member != "" {
		subject = fmt.Sprintf("whose member %s", hold.Member)
	}
	// What a collect of the name would do differs by holder, and only the
	// two kinds with a finished-or-running job behind them can be read at all:
	// the caller then gets a job that was already there — the holder, or an
	// older one of its own — so the sentence claims no more than "earlier".
	earlier := fmt.Sprintf(" Collecting %s now reads an earlier job, not a new one.", name)
	var state, release string
	switch hold.Kind {
	case HoldRunning:
		state = "is still running"
		release = earlier + " The name frees once that job has finished and its own caller has collected it."
	case HoldUncollected:
		state = "finished but has not been collected"
		release = earlier + " If that job is yours or its caller is gone, collecting it frees the name: " + CollectCommand(dir)
	case HoldUnrecorded:
		switch {
		case hold.Member != "":
			state = "has written no record while the process running that fan-out may still be alive, so it may yet start"
			release = earlier + " The name frees once that fan-out has finished and its own caller has collected it."
		case hold.Empty:
			state = "was reserved and holds no record — a dispatch in its first moments, or one that died there, and nothing here says which"
			release = " Once you have established that no envoy run owns that directory and it is still empty, removing it frees the name."
		default:
			state = "was reserved and holds no record — a dispatch in its first moments, or one that died there, and nothing here says which"
			release = " Read what that directory holds, and establish that no envoy run owns it, before anything reuses its name."
		}
	default:
		state = "holds a record that cannot be read"
		release = " Inspect that directory before anything reuses its name."
	}
	return fmt.Sprintf("nothing was dispatched: the name %s is held by another job, %s, %s %s. "+
		"Dispatch again under a different name (%s-b, say).%s",
		name, dir, subject, state, name, release)
}

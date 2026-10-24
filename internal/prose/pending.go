package prose

import (
	"fmt"
	"strings"

	"github.com/qiushiyan/envoy/internal/job"
)

// Pending discovery is the recovery index a caller reads after a completion it
// may have missed: why each job still needs attention, and the one action for
// it.

// PendingUnreadable is why a job whose record will not read needs attention.
func PendingUnreadable(record string, err error) string {
	return fmt.Sprintf("%s could not be read: %s", record, err)
}

// PendingUnreadableNext is the one action for a job whose record will not
// read: the logs and the tree are the evidence left.
func PendingUnreadableNext(dir string) string {
	ws := job.Workspace{Dir: dir}
	return fmt.Sprintf("inspect %s, %s, %s, and the working tree; do not infer completion from the damaged metadata",
		ws.ProgressLogPath(), ws.RawLogPath(), ws.StderrLogPath())
}

// PendingOtherSchema is why an uncollected job written by another engine
// version is listed: its record — meta.json, or a fan-out's group.json — is
// intact, and this engine will not reinterpret it.
func PendingOtherSchema(record string, version int, reads []int) string {
	return fmt.Sprintf("its %s is schema %d, written by another envoy version; this one reads schema %s only and will not reinterpret it, so it cannot deliver the job", record, version, job.SchemaList(reads))
}

// PendingOtherSchemaNext is the one action for another version's job: its
// files are intact, and they are the evidence.
func PendingOtherSchemaNext(dir string) string {
	ws := job.Workspace{Dir: dir}
	return fmt.Sprintf("read %s and %s directly, or collect it with the envoy version that wrote it", ws.ResultPath(), ws.MetaPath())
}

// PendingOtherSchemaFanNext is the same action for another version's
// fan-out, whose results sit one level down, in its members' directories.
func PendingOtherSchemaFanNext(dir string) string {
	return fmt.Sprintf("read each member's result.md and meta.json under %s directly, or collect it with the envoy version that wrote it", dir)
}

// PendingRunning is why a job still running is listed: its caller may have
// stopped waiting on it, so it may be owed a wait.
func PendingRunning(observed string) string {
	return "still running in its own process (" + observed + "); if your dispatch of it is no longer running, nothing will tell you when it ends"
}

// PendingFanAlive is what a fan-out still running shows: the one process
// every member runs in.
func PendingFanAlive() string {
	return "the process running every member is alive"
}

// PendingRunningNext is the one action for a running job a caller may have
// lost: a wait, in the background, whose exit is the completion signal.
func PendingRunningNext(dir string) string {
	return "run this in the background and collect the job once it exits: " + WaitCommand(dir)
}

// PendingUncollected is why a finished job needs attention.
func PendingUncollected(status string) string {
	return fmt.Sprintf("terminal status %s has not been collected", status)
}

// PendingMemberNoRecord is why a roster member with no record needs
// attention: the roster is the only evidence it was meant to run.
func PendingMemberNoRecord() string {
	return "has no meta.json, so it never recorded a start"
}

// PendingMembers rolls a fan-out's members up as one entry: one collect
// covers them all.
func PendingMembers(reasons []string, total int) string {
	return fmt.Sprintf("%d of %d members still need attention — %s", len(reasons), total, strings.Join(reasons, " · "))
}

// PendingNone closes an index with nothing in it.
func PendingNone() string {
	return "no recovery action is needed"
}

package collect

import (
	"errors"
	"fmt"
	"os"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
)

// Unresolvable words the two ways a name can fail to resolve that are the
// store's fault rather than the caller's — the job's own name, a continued
// voice's, or a read's: each stops with exit 2 instead of letting an older
// job answer to the name.
func Unresolvable(err error) (string, bool) {
	if unreadable, ok := errors.AsType[*job.StoreUnreadableError](err); ok {
		return prose.UnreadableStore(unreadable.Base, unreadable.Err), true
	}
	if unattributed, ok := errors.AsType[*job.UnattributedError](err); ok {
		return prose.UnattributedGeneration(unattributed.Name, unattributed.Dir, unattributed.Err), true
	}
	return "", false
}

// resolve resolves the job a read names — a name in this project's store, or
// a directory — to the directory it means to this caller, and the note a name
// that fell back to another caller's job carries: the engine cannot tell work
// picked up on purpose from a caller whose identity changed, so it reports
// which happened and leaves the judgment. A ref that resolves to nothing comes
// back as err, unprinted, with its exit code.
func resolve(ref string) (dir, note string, code int, err error) {
	invocationCwd, err := os.Getwd()
	if err != nil {
		return "", "", job.ExitInfra, fmt.Errorf("cannot determine cwd: %s", err)
	}
	resolved, err := job.ResolveRef(ref, invocationCwd, job.CallerFromEnv())
	if msg, unres := Unresolvable(err); unres {
		return "", "", job.ExitInfra, errors.New(msg)
	}
	if err != nil {
		return "", "", job.ExitUsage, err
	}
	if resolved.FellBack {
		note = prose.NameFellBack(resolved.Name, resolved.Owner != "")
	}
	return resolved.Dir, note, 0, nil
}

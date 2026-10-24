package collect

import (
	"errors"
	"fmt"
	"io"
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
// which happened and leaves the judgment. verb labels its errors and missing
// words the refusal of an empty job; ok false means code is the exit.
func resolve(arg, verb, missing string, stderr io.Writer) (dir, note string, code int, ok bool) {
	invocationCwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "%s error: cannot determine cwd: %s\n", verb, err)
		return "", "", job.ExitInfra, false
	}
	if arg == "" {
		fmt.Fprintf(stderr, "usage error: %s\n", missing)
		return "", "", job.ExitUsage, false
	}
	ref, err := job.ResolveRef(arg, invocationCwd, job.CallerFromEnv())
	if msg, unres := Unresolvable(err); unres {
		fmt.Fprintf(stderr, "%s error: %s\n", verb, msg)
		return "", "", job.ExitInfra, false
	}
	if err != nil {
		fmt.Fprintf(stderr, "usage error: %s\n", err)
		return "", "", job.ExitUsage, false
	}
	if ref.FellBack {
		note = prose.NameFellBack(ref.Name, ref.Owner != "")
	}
	return ref.Dir, note, 0, true
}

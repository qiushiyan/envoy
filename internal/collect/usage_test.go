package collect

import (
	"slices"
	"testing"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
)

func TestAbandonedUsagePreservesTheLastObservation(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		dir := t.TempDir()
		u := job.NewUsage()
		u.LatestContextTokens, u.PeakContextTokens = job.Ptr(int64(100)), job.Ptr(int64(200))
		u.Final = terminal
		if terminal {
			u.OutputTokens = job.Ptr(int64(42))
		}
		u.RefreshState()
		m := &job.Meta{SchemaVersion: job.MetaSchemaVersion, Status: job.StatusRunning, Usage: u}
		reconcileAbandoned(dir, m, prose.RunObservation{State: prose.RunAbandoned})
		got, err := job.ReadMetaFile(job.Workspace{Dir: dir}.MetaPath())
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != job.StatusAbandoned || !got.Usage.Final || *got.Usage.LatestContextTokens != 100 || *got.Usage.PeakContextTokens != 200 {
			t.Fatalf("reconciliation erased usage: %+v", got)
		}
		if terminal {
			if got.Usage.State != "settled" || *got.Usage.OutputTokens != 42 {
				t.Fatal("reconciliation replaced terminal usage")
			}
		} else if got.Usage.State != "incomplete" || !slices.Contains(got.Usage.Issues, "missing_terminal") {
			t.Fatal("reconciliation left usage live")
		}
	}
}

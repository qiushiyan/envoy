package envoy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiushiyan/envoy/internal/job"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

// collectedAt means the answer reached a caller. A writer that accepts none
// of it delivered nothing, so the job must stay owed and the exit code must
// say the collection failed.
func TestCollectStampsOnlyWhatReachedTheCaller(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "job")
	ws := job.Workspace{Dir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := &job.Meta{SchemaVersion: job.MetaSchemaVersion, Status: job.StatusOK, Provider: "codex",
		SessionID: job.Ptr("sess-1"), PromptState: job.PromptAccepted, TimeoutMin: 5, ResultKind: job.ResultFinal}
	if err := meta.WriteFile(ws.MetaPath()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(ws.ResultPath(), []byte("the answer"), 0o644)

	var errBuf bytes.Buffer
	if code := Collect(CollectRequest{Job: dir, Stdout: failingWriter{}, Stderr: &errBuf}); code == 0 {
		t.Fatalf("collect into a dead writer reported success\nstderr:\n%s", errBuf.String())
	}
	got, err := job.ReadMetaFile(ws.MetaPath())
	if err != nil {
		t.Fatal(err)
	}
	if got.CollectedAt != nil {
		t.Fatal("nothing was delivered, so nothing may be marked collected")
	}

	var out bytes.Buffer
	if code := Collect(CollectRequest{Job: dir, Stdout: &out, Stderr: &errBuf}); code != 0 {
		t.Fatalf("collect = %d\n%s", code, errBuf.String())
	}
	if got, _ := job.ReadMetaFile(ws.MetaPath()); got.CollectedAt == nil {
		t.Fatal("a delivered result stamps collection")
	}
}

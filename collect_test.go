package envoy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/envoy/internal/collect"
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
		SessionID: new("sess-1"), PromptState: job.PromptAccepted, TimeoutMin: 5, ResultKind: job.ResultFinal}
	if err := meta.WriteFile(ws.MetaPath()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(ws.ResultPath(), []byte("the answer"), 0o644)

	var errBuf bytes.Buffer
	if code := Collect(CollectRequest{Job: dir, Stdout: failingWriter{}, Stderr: &errBuf}); code == 0 {
		t.Fatalf("collect into a dead writer reported success\nstderr:\n%s", errBuf.String())
	}
	got, err := job.ReadMeta(dir)
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
	if got, _ := job.ReadMeta(dir); got.CollectedAt == nil {
		t.Fatal("a delivered result stamps collection")
	}
}

// A fan-out is delivered as one block, so no member is stamped until the whole
// block reached the caller: a broken write leaves every member owed, and the
// write that arrives stamps them all. A note on how its name resolved sits
// under the block's first line, as it does for a single turn.
func TestCollectStampsAFanOutOnlyOnceItsBlockArrived(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "fan")
	members := []string{"codex", "claude"}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	group := &job.Group{SchemaVersion: job.GroupSchemaVersion, Members: members, TimeoutMin: 5}
	if err := group.WriteFile(job.GroupWorkspace{Dir: dir}.GroupPath()); err != nil {
		t.Fatal(err)
	}
	for _, name := range members {
		ws := job.GroupWorkspace{Dir: dir}.Member(name)
		if err := os.MkdirAll(ws.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := &job.Meta{SchemaVersion: job.MetaSchemaVersion, Status: job.StatusOK, Provider: name,
			SessionID: new("sess-" + name), PromptState: job.PromptAccepted, TimeoutMin: 5, ResultKind: job.ResultFinal}
		if err := meta.WriteFile(ws.MetaPath()); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(ws.ResultPath(), []byte("the answer from "+name), 0o644)
	}
	stamped := func() (n int) {
		for _, name := range members {
			if m, err := job.ReadMeta(job.GroupWorkspace{Dir: dir}.Member(name).Dir); err == nil && m.CollectedAt != nil {
				n++
			}
		}
		return n
	}

	var errBuf bytes.Buffer
	if code := collect.Collect(dir, "", collect.ModeFull, failingWriter{}, &errBuf); code == 0 {
		t.Fatalf("collect into a dead writer reported success\nstderr:\n%s", errBuf.String())
	}
	if n := stamped(); n != 0 {
		t.Fatalf("%d members marked collected, but nothing was delivered", n)
	}

	var out bytes.Buffer
	if code := collect.Collect(dir, "picked up", collect.ModeFull, &out, &errBuf); code != 0 {
		t.Fatalf("collect = %d\n%s", code, errBuf.String())
	}
	if !strings.HasPrefix(out.String(), "fan-out: "+dir+"\nnote: picked up\n") {
		t.Fatalf("the note must sit under the block's first line:\n%s", out.String())
	}
	if n := stamped(); n != len(members) {
		t.Fatalf("%d of %d members marked collected after the block arrived", n, len(members))
	}
}

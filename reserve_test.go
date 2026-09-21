package envoy

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Dispatches racing for a delivered name: exactly one takes the next
// generation, and every loser is told the name is held — the winner's
// reservation is what holds it — rather than handed an error or, worse, a
// generation of its own that the name would never resolve to.
func TestReserveRacingForADeliveredName(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "review-r1")
	if err := os.Mkdir(first, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"schemaVersion":9,"status":"ok","provider":"codex","promptState":"accepted","timeoutMin":5,"collectedAt":"2026-09-21T00:00:00.000Z"}`
	if err := os.WriteFile(filepath.Join(first, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	const n = 32
	type outcome struct {
		dir, refusal string
		err          error
	}
	outcomes := make([]outcome, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			o := &outcomes[i]
			o.dir, o.refusal, o.err = reserve("review-r1", first)
		})
	}
	close(start)
	wg.Wait()

	won := 0
	for i, o := range outcomes {
		switch {
		case o.err != nil:
			t.Fatalf("dispatch %d: a lost race must be a refusal, got error %v", i, o.err)
		case o.dir != "":
			won++
			if o.dir != first+"+2" {
				t.Fatalf("dispatch %d reserved %q, want %q", i, o.dir, first+"+2")
			}
		case !strings.Contains(o.refusal, "nothing was dispatched") || !strings.Contains(o.refusal, first+"+2"):
			t.Fatalf("dispatch %d refusal = %q, want the name held by %s", i, o.refusal, first+"+2")
		}
	}
	if won != 1 {
		t.Fatalf("%d dispatches reserved a directory, want exactly 1", won)
	}
	entries, _ := os.ReadDir(base)
	if len(entries) != 2 {
		t.Fatalf("store holds %d directories, want review-r1 and review-r1+2 only", len(entries))
	}
}

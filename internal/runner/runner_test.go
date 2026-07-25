package runner

import (
	"strings"
	"testing"
	"time"
)

// The hard cap must be a wall-clock deadline: Go's monotonic readings freeze
// during laptop sleep (mach_absolute_time on darwin), so a deadline carrying
// one silently stretches the cap across a suspend — the exact failure the JS
// engine's Date.now() comparison was built to prevent. A time.Time formats
// with an " m=±…" suffix iff it still carries a monotonic reading.
func TestDeadlineIsWallClock(t *testing.T) {
	d := deadlineFrom(time.Now(), 30)
	if strings.Contains(d.String(), " m=") {
		t.Fatalf("deadline carries a monotonic reading; laptop sleep would stretch the cap: %v", d)
	}
	if !deadlineFrom(time.Now(), 0).IsZero() {
		t.Fatal("timeout 0 must mean no deadline")
	}
	want := 90 * time.Second
	got := deadlineFrom(time.Now(), 1.5).Sub(time.Now().Round(0)).Round(time.Second)
	if got != want {
		t.Fatalf("deadline offset = %v, want %v", got, want)
	}
}

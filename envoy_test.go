package envoy

import (
	"math"
	"testing"
)

// The zero value of a safety cap must be the safe default, not "uncapped":
// envoy.RunRequest{Job: "j", With: []string{"codex"}, PromptFile: "x.md"}
// must not compile into an unbounded turn. Uncapped requires the explicit
// NoTimeout.
func TestResolveTimeoutZeroValueIsSafe(t *testing.T) {
	cases := []struct {
		name    string
		req     RunRequest
		want    float64
		wantErr bool
	}{
		{name: "zero value defaults to the safety cap", req: RunRequest{}, want: 30},
		{name: "explicit cap wins", req: RunRequest{TimeoutMin: 5}, want: 5},
		{name: "uncapped requires NoTimeout", req: RunRequest{NoTimeout: true}, want: 0},
		{name: "negative rejected", req: RunRequest{TimeoutMin: -1}, wantErr: true},
		{name: "NaN rejected", req: RunRequest{TimeoutMin: math.NaN()}, wantErr: true},
		{name: "conflicting pair rejected", req: RunRequest{NoTimeout: true, TimeoutMin: 5}, wantErr: true},
	}
	for _, c := range cases {
		got, err := resolveTimeout(c.req.TimeoutMin, c.req.NoTimeout)
		if c.wantErr != (err != nil) {
			t.Fatalf("%s: err = %v", c.name, err)
		}
		if err == nil && got != c.want {
			t.Fatalf("%s: timeout = %g, want %g", c.name, got, c.want)
		}
	}
}

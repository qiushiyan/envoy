package envoy

import (
	"math"
	"testing"
)

// The zero value of a safety cap must be the safe default, not "uncapped":
// envoy.TurnRequest{Provider: "codex", PromptFile: "x.md"} used to compile
// into an unbounded turn. Uncapped now requires the explicit NoTimeout.
func TestResolveTimeoutZeroValueIsSafe(t *testing.T) {
	cases := []struct {
		name    string
		req     TurnRequest
		want    float64
		wantErr bool
	}{
		{name: "zero value defaults to the safety cap", req: TurnRequest{}, want: 30},
		{name: "explicit cap wins", req: TurnRequest{TimeoutMin: 5}, want: 5},
		{name: "uncapped requires NoTimeout", req: TurnRequest{NoTimeout: true}, want: 0},
		{name: "negative rejected", req: TurnRequest{TimeoutMin: -1}, wantErr: true},
		{name: "NaN rejected", req: TurnRequest{TimeoutMin: math.NaN()}, wantErr: true},
		{name: "conflicting pair rejected", req: TurnRequest{NoTimeout: true, TimeoutMin: 5}, wantErr: true},
	}
	for _, c := range cases {
		got, err := resolveTimeout(c.req)
		if c.wantErr != (err != nil) {
			t.Fatalf("%s: err = %v", c.name, err)
		}
		if err == nil && got != c.want {
			t.Fatalf("%s: timeout = %g, want %g", c.name, got, c.want)
		}
	}
}

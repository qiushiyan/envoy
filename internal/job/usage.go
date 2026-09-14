package job

import "slices"

// Usage measures this job's primary turn, never a session's lifetime totals.
// Context is the input to the latest observed response, not a prediction of
// the next request's size. Final means measurement ended, not that it is complete.
// A killed runner can leave its last non-final snapshot on disk.
type Usage struct {
	State                    string       `json:"state"` // unmeasured | live | settled | incomplete
	Final                    bool         `json:"final"`
	Attribution              string       `json:"attribution"`     // unknown | complete | incomplete
	UnexpectedModel          *string      `json:"unexpectedModel"` // last unexpected model; issues remain sticky
	Issues                   []string     `json:"issues"`
	LatestContextTokens      *int64       `json:"latestContextTokens"`
	PeakContextTokens        *int64       `json:"peakContextTokens"`
	Responses                int64        `json:"responses"` // unique attributed primary response IDs
	SampledAt                *string      `json:"sampledAt"` // Envoy receipt time of the last valid context sample
	InputTokens              *int64       `json:"inputTokens"`
	CacheReadInputTokens     *int64       `json:"cacheReadInputTokens"`
	CacheCreationInputTokens *int64       `json:"cacheCreationInputTokens"`
	OutputTokens             *int64       `json:"outputTokens"` // nil until a complete, reconciled terminal total
	ContextWindowTokens      *int64       `json:"contextWindowTokens"`
	TerminalTokens           *UsageTokens `json:"terminalTokens,omitempty"` // provider totals retained even when incomplete or inconsistent
}

// UsageTokens is the terminal provider report, separate from the accepted
// totals above so a contradictory or truncated envelope cannot erase evidence.
type UsageTokens struct {
	Input         *int64 `json:"input"`
	CacheRead     *int64 `json:"cacheRead"`
	CacheCreation *int64 `json:"cacheCreation"`
	Output        *int64 `json:"output"`
}

func NewUsage() *Usage {
	return &Usage{State: "unmeasured", Attribution: "unknown", Issues: []string{}}
}

// AddIssue retains each kind of missing evidence once. Callers use a finite
// vocabulary, keeping snapshots fixed in size even for a long stream.
func (u *Usage) AddIssue(issue string) {
	if !slices.Contains(u.Issues, issue) {
		u.Issues = append(u.Issues, issue)
	}
}

func (u *Usage) RefreshState() {
	switch {
	case u.Final && len(u.Issues) > 0:
		u.State = "incomplete"
	case u.Final:
		u.State = "settled"
	case u.LatestContextTokens != nil:
		u.State = "live"
	default:
		u.State = "unmeasured"
	}
}

// End records finalization without a provider envelope. A terminal observation
// already settled by the driver wins over subsequent process cleanup.
func (u *Usage) End() {
	if u == nil || u.Final {
		return
	}
	u.Final = true
	u.AddIssue("missing_terminal")
	u.RefreshState()
}

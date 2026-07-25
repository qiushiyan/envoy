// Package provider is the extensibility seam of the engine: everything one
// provider CLI needs — argv, environment, stream-event parsing, recovery
// evidence, terminal-outcome assembly — lives in that provider's driver, and
// the runner consumes only the semantic events defined here. Adding a
// provider means adding one driver file, not touching the lifecycle.
package provider

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
)

// Spec is one turn as requested by the caller, provider-agnostic.
type Spec struct {
	Provider     string
	PromptFile   string
	Model        string // "" = the provider's own configured default
	Effort       string // "" = the provider's own configured default
	Resume       string // session id to continue, "" = fresh session
	AllowWrite   bool
	Cwd          string
	TimeoutMin   float64
	MaxBudgetUSD *float64 // claude only
	Baseline     string
	Label        string
}

// EventKind classifies the semantic events a driver emits from raw stream
// lines. The runner never inspects provider-specific payloads.
type EventKind int

const (
	// KindActivity: one provider event was observed (feeds counters/heartbeat).
	KindActivity EventKind = iota
	// KindSessionStarted: the provider revealed a fresh session id mid-stream.
	KindSessionStarted
	// KindAccepted: this turn provably reached model work.
	KindAccepted
	// KindTerminal: an authoritative provider outcome envelope was observed.
	KindTerminal
	// KindNote: a provider milestone worth a progress-log line.
	KindNote
)

type Event struct {
	Kind      EventKind
	Type      string   // KindActivity: raw provider event type
	SessionID string   // KindSessionStarted
	Evidence  string   // KindAccepted
	Terminal  string   // KindTerminal: label, e.g. "claude success"
	State     string   // KindNote: progress state name
	Fields    []job.KV // KindNote
}

// Evidence is what a driver can prove about an abnormally ended turn.
type Evidence struct {
	Accepted bool
	Label    string  // how acceptance was proven, "" if it wasn't
	Partial  *string // recovered output; nil means none was observed
	Tokens   *job.Tokens
	CostUSD  *float64
}

// ExitInfo describes how the provider process ended.
type ExitInfo struct {
	Code         *int
	Signal       *string
	Terminated   bool   // a stop (timeout/interrupt) was requested this turn
	TerminalType string // observed provider terminal label, "" if none
	StderrTail   string // last ~2000 chars of provider stderr
}

// Outcome is a driver's normal-path conclusion after process end.
type Outcome struct {
	Status              string // job.StatusOK / StatusFailed / StatusInfra
	Text                string // final text (ok only)
	ErrorText           string
	NextAction          string // recovery action; "" lets finish() use its default
	Partial             *string
	Tokens              *job.Tokens
	CostUSD             *float64
	PromptState         string  // "" = keep the current recorded state
	PromptStateEvidence *string // set only when HasEvidence
	HasEvidence         bool
	SessionID           string // "" = unchanged; claude's result envelope can override
}

// Driver runs one provider turn's protocol. Drivers are stateful: Feed
// accumulates the stream, and Recovery/Conclude read that accumulation.
// All methods are called from the runner's single event loop.
type Driver interface {
	Name() string
	Efforts() []string
	// PreflightSessionID is the session id known before spawn ("" when the
	// provider only reveals it mid-stream, like a fresh codex thread).
	PreflightSessionID() string
	Argv(ws job.Workspace) []string
	ExtraEnv() []string
	Feed(line string) []Event
	// Poll runs on each heartbeat for out-of-band evidence gathering.
	Poll() []Event
	// Recovery reports what can be proven after an abnormal ending. Returned
	// events (e.g. a late acceptance) must be processed by the runner first.
	Recovery() (Evidence, []Event)
	// Conclude assembles the outcome for a normally-ended process.
	Conclude(exit ExitInfo) Outcome
	// ResumeArgs is the caller-facing resume fragment incl. the current cap.
	ResumeArgs() string
	Takeover() string
}

// New constructs the driver for spec.Provider.
func New(spec Spec, ws job.Workspace, startedAt time.Time) (Driver, error) {
	switch spec.Provider {
	case "claude":
		return newClaude(spec, startedAt), nil
	case "codex":
		return newCodex(spec, ws), nil
	default:
		return nil, fmt.Errorf("--provider must be claude or codex, got '%s'", spec.Provider)
	}
}

var efforts = map[string][]string{
	"claude": {"low", "medium", "high", "xhigh", "max"},
	"codex":  {"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
}

// ValidateEffort rejects an invalid effort BEFORE spawn: claude silently
// ignores an invalid effort (runs at default), codex burns a turn-start and
// fails with an API 400 mid-stream.
func ValidateEffort(providerName, effort string) error {
	valid, ok := efforts[providerName]
	if !ok {
		return fmt.Errorf("--provider must be claude or codex, got '%s'", providerName)
	}
	if effort == "" {
		return nil
	}
	for _, v := range valid {
		if v == effort {
			return nil
		}
	}
	hint := ""
	if providerName == "claude" && (effort == "none" || effort == "minimal") {
		hint = fmt.Sprintf(" (claude has no '%s'; its lowest is 'low')", effort)
	}
	return fmt.Errorf("--effort '%s' is not valid for %s. Valid: %s%s",
		effort, providerName, strings.Join(valid, ", "), hint)
}

func resumeArgs(sessionID string, timeoutMin float64) string {
	if sessionID == "" {
		return ""
	}
	return fmt.Sprintf("--resume %s --timeout-min %g", sessionID, timeoutMin)
}

// stderrDetail condenses a stderr tail into the last three non-empty lines.
func stderrDetail(tail string) string {
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(tail), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return "(no stderr detail)"
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.Join(lines, " | ")
}

// firstText returns the first candidate with non-empty content, as a pointer,
// or nil — mirroring JS's `a || b || c || undefined` chains over strings.
func firstText(candidates ...*string) *string {
	for _, c := range candidates {
		if c != nil && *c != "" {
			return c
		}
	}
	return nil
}

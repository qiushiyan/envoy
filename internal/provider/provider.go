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

// Options is the provider-facing slice of a turn request: exactly the fields
// a driver reads. Runner-only concerns (prompt file, cwd, baseline, label)
// deliberately do not ride through this seam.
type Options struct {
	Model        string // "" = the provider's own configured default
	Effort       string // "" = the provider's own configured default
	Resume       string // session id to continue, "" = fresh session
	AllowWrite   bool
	TimeoutMin   float64  // recorded into resume fragments
	MaxBudgetUSD *float64 // claude only
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
	// KindModelReported: the provider announced the model it resolved for
	// this turn. An observation of the provider's own statement — the engine
	// still never infers what an alias or an omitted --model means.
	KindModelReported
)

type Event struct {
	Kind      EventKind
	Type      string   // KindActivity: raw provider event type
	SessionID string   // KindSessionStarted
	Evidence  string   // KindAccepted
	Terminal  string   // KindTerminal: label, e.g. "claude success"
	State     string   // KindNote: progress state name
	Fields    []job.KV // KindNote
	Model     string   // KindModelReported
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

// Outcome is a driver's normal-path conclusion after process end. A driver
// reports what it observed — the cause, and any cause-specific fix the caller
// must apply first — and never the recovery prescription itself: that follows
// from the prompt state and is worded once in internal/prose.
type Outcome struct {
	Status              string // job.StatusOK / StatusFailed / StatusInfra
	Text                string // final text (ok only)
	ErrorText           string // what this provider reported or exited with
	Remedy              string // cause-specific fix, e.g. "Raise the budget cap first."; "" when none
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
	// PreflightSessionID is the session id known before spawn ("" when the
	// provider only reveals it mid-stream, like a fresh codex thread).
	PreflightSessionID() string
	Argv() []string
	ExtraEnv() []string
	Feed(line string) []Event
	// Poll runs on each heartbeat for out-of-band evidence gathering.
	Poll() []Event
	// Recovery reports what can be proven after an abnormal ending. Returned
	// events (e.g. a late acceptance) must be processed by the runner first.
	Recovery() (Evidence, []Event)
	// Conclude assembles the outcome for a normally-ended process.
	Conclude(exit ExitInfo) Outcome
	// Takeover is the provider's own command for continuing this session
	// interactively, once the turn is terminal.
	Takeover() string
}

// New constructs the named provider's driver.
func New(name string, opts Options, ws job.Workspace, startedAt time.Time) (Driver, error) {
	switch name {
	case "claude":
		return newClaude(opts, startedAt), nil
	case "codex":
		return newCodex(opts, ws), nil
	default:
		return nil, fmt.Errorf("--provider must be claude or codex, got '%s'", name)
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

// EffortList is the provider's own effort vocabulary, so the CLI's help text
// and its validation cannot drift apart.
func EffortList(providerName string) []string { return efforts[providerName] }

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

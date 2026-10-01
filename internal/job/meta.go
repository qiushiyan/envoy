package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ConnectionErrors is the observed tally of a provider's connection-error
// events: how many, and when the first and last arrived.
type ConnectionErrors struct {
	Count   int64   `json:"count"`
	FirstAt *string `json:"firstAt"`
	LastAt  *string `json:"lastAt"`
}

// Tokens carries provider usage in whichever fields the provider reports.
// Field order fixes the display order in collect.
type Tokens struct {
	Input           *int64 `json:"input,omitempty"`
	CacheRead       *int64 `json:"cacheRead,omitempty"`
	CacheCreation   *int64 `json:"cacheCreation,omitempty"`
	CachedInput     *int64 `json:"cachedInput,omitempty"`
	Output          *int64 `json:"output,omitempty"`
	ReasoningOutput *int64 `json:"reasoningOutput,omitempty"`
}

// Pairs returns the set fields in display order.
func (t *Tokens) Pairs() [][2]any {
	if t == nil {
		return nil
	}
	var out [][2]any
	add := func(k string, v *int64) {
		if v != nil {
			out = append(out, [2]any{k, *v})
		}
	}
	add("input", t.Input)
	add("cacheRead", t.CacheRead)
	add("cacheCreation", t.CacheCreation)
	add("cachedInput", t.CachedInput)
	add("output", t.Output)
	add("reasoningOutput", t.ReasoningOutput)
	return out
}

// Meta is the single machine-readable source of lifecycle and recovery truth
// for one turn. It exists from turn start (status "running") and every update
// is an atomic replace, so a killed job still leaves coordinates on disk.
//
// It records facts, never prose: the commands and prescriptions a caller
// reads are rendered from these fields at collect time, by the engine that
// is reading them. Paths inside the job directory are not recorded either —
// the directory a record was read from is the one it describes.
type Meta struct {
	SchemaVersion int      `json:"schemaVersion"`
	Status        string   `json:"status"`
	Provider      string   `json:"provider"`
	Model         *string  `json:"model"`  // nil = the provider's own configured default
	Effort        *string  `json:"effort"` // nil = the provider's own configured default
	Cwd           string   `json:"cwd"`
	AllowWrite    bool     `json:"allowWrite"`
	GitBaseline   *string  `json:"gitBaseline"`
	MaxBudgetUSD  *float64 `json:"maxBudgetUsd"` // the spend cap this turn was dispatched with; nil = none
	SessionID     *string  `json:"sessionId"`
	// ResumedFrom is the job dir whose conversation this turn continues;
	// absent for a fresh conversation. It is the source a faithful
	// re-dispatch names again.
	ResumedFrom *string `json:"resumedFrom,omitempty"`
	// Caller is the dispatching session's identity as its harness exported
	// it; absent when none was exported, and in older records. It scopes what
	// a job name means to that caller and is never inherited by a continuation.
	Caller              *string       `json:"caller,omitempty"`
	SessionLockConflict *LockConflict `json:"sessionLockConflict"`
	StartedAt           string        `json:"startedAt"`
	EndedAt             *string       `json:"endedAt"`
	DurationMs          *int64        `json:"durationMs"`
	TimeoutMin          float64       `json:"timeoutMin"`
	DeadlineAt          *string       `json:"deadlineAt"`
	ProviderArgv        []string      `json:"providerArgv"`
	// CommandPrefix is the resolved executable and launcher arguments. The
	// executed argv is CommandPrefix + ProviderArgv[1:]; ProviderArgv keeps
	// its provider-native meaning. Absent in older records means unknown.
	// This observation is never inherited by a continuation.
	CommandPrefix             []string `json:"commandPrefix,omitempty"`
	RunnerPid                 int      `json:"runnerPid"`
	RunnerInstanceID          string   `json:"runnerInstanceId"`
	ProviderPid               *int     `json:"providerPid"`
	ProviderPgid              *int     `json:"providerPgid"`
	ChildExitCode             *int     `json:"childExitCode"`
	ChildExitSignal           *string  `json:"childExitSignal"`
	PromptState               string   `json:"promptState"`
	PromptStateEvidence       *string  `json:"promptStateEvidence"`
	PromptAcceptedAt          *string  `json:"promptAcceptedAt"`
	TerminationRequestedAt    *string  `json:"terminationRequestedAt"`
	InterruptionSignal        *string  `json:"interruptionSignal"`
	LastHeartbeatAt           *string  `json:"lastHeartbeatAt"`
	ProviderTerminalAt        *string  `json:"providerTerminalAt"`
	ProviderTerminalEventType *string  `json:"providerTerminalEventType"`
	ProviderOutputBytes       int64    `json:"providerOutputBytes"`
	ProviderEventCount        int64    `json:"providerEventCount"`
	LastProviderOutputAt      *string  `json:"lastProviderOutputAt"`
	LastProviderActivityAt    *string  `json:"lastProviderActivityAt"`
	LastProviderEventType     *string  `json:"lastProviderEventType"`
	ProviderReportedModel     *string  `json:"providerReportedModel"` // the provider's own announcement; never inferred
	// ConnectionErrors counts the provider's own connection-error events over
	// the turn. Non-nil only for a driver that observes them (a zero count is
	// a real observation); nil means the driver never looked.
	ConnectionErrors *ConnectionErrors `json:"connectionErrors"`
	Tokens           *Tokens           `json:"tokens"`
	Usage            *Usage            `json:"usage,omitempty"`
	CostUSD          *float64          `json:"costUsd"`
	// Failure is why a turn that did not deliver ended; nil for an ok turn.
	Failure      *Failure `json:"failure"`
	ResultKind   string   `json:"resultKind"`
	CollectedAt  *string  `json:"collectedAt"`
	ReconciledAt *string  `json:"reconciledAt,omitempty"`
}

// MetaSchemaVersion is the only schema this engine reads or writes. Records
// from another version are refused rather than reinterpreted: a job store
// holds one engine's records at a time.
// Optional additive observations (such as usage) retain this version; absence
// means unavailable, and existing fields keep their semantics. Schema 10
// replaced the worded error, remedy and lock-conflict strings with the
// failure and lock-conflict observations they were worded from.
const MetaSchemaVersion = 10

// UsesLauncher reports whether a non-bare command was recorded.
// Absent and bare prefixes both leave nothing to report about a launcher.
func (m *Meta) UsesLauncher() bool {
	return len(m.CommandPrefix) > 1 || (len(m.CommandPrefix) == 1 && m.CommandPrefix[0] != m.Provider)
}

// Marshal renders the canonical on-disk form.
func (m *Meta) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// WriteFile atomically replaces the job's meta.json.
func (m *Meta) WriteFile(path string) error {
	data, err := m.Marshal()
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data)
}

// ErrNoRecord reports a directory without the record that makes it a job: a
// reservation before its first write, or a path that never was one. It is
// absence, not damage — a record that is there and will not read comes back
// as its own error, never as this one.
var ErrNoRecord = errors.New("no record")

// ReadMeta reads the turn record in dir, ErrNoRecord when it has none.
func ReadMeta(dir string) (*Meta, error) {
	meta, err := ReadMetaFile(Workspace{Dir: dir}.MetaPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoRecord
	}
	return meta, err
}

// ReadMetaFile parses a meta.json this engine wrote.
func ReadMetaFile(path string) (*Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	if meta.SchemaVersion != MetaSchemaVersion {
		return nil, fmt.Errorf("meta.json is schema %d and this engine reads schema %d only", meta.SchemaVersion, MetaSchemaVersion)
	}
	return &meta, nil
}

// Ptr is a convenience for the schema's many nullable fields.
func Ptr[T any](v T) *T { return &v }

// PtrIfNonEmpty records an optional string field: "" is unset, so it is nil.
func PtrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Deref reads an optional string field, "" when it is unset.
func Deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

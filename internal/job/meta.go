package job

import (
	"encoding/json"
	"fmt"
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
	ResumedFrom               *string  `json:"resumedFrom,omitempty"`
	SessionLockConflict       *string  `json:"sessionLockConflict"`
	StartedAt                 string   `json:"startedAt"`
	EndedAt                   *string  `json:"endedAt"`
	DurationMs                *int64   `json:"durationMs"`
	TimeoutMin                float64  `json:"timeoutMin"`
	DeadlineAt                *string  `json:"deadlineAt"`
	ProviderArgv              []string `json:"providerArgv"`
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
	Error            *string           `json:"error"`
	// Remedy is the driver's cause-specific fix for a turn that did not
	// deliver ("Raise the budget cap first."); nil when the cause needs none.
	Remedy       *string `json:"remedy"`
	ResultKind   string  `json:"resultKind"`
	CollectedAt  *string `json:"collectedAt"`
	ReconciledAt *string `json:"reconciledAt,omitempty"`
}

// MetaSchemaVersion is the only schema this engine reads or writes. Records
// from another version are refused rather than reinterpreted: a job store
// holds one engine's records at a time.
// Optional additive observations (such as usage) retain this version; absence
// means unavailable, and existing fields keep their semantics.
const MetaSchemaVersion = 9

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

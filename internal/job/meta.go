package job

import (
	"encoding/json"
	"os"
)

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
type Meta struct {
	SchemaVersion             int      `json:"schemaVersion"`
	Status                    string   `json:"status"`
	Provider                  string   `json:"provider"`
	Model                     *string  `json:"model"`  // nil = the provider's own configured default
	Effort                    *string  `json:"effort"` // nil = the provider's own configured default
	Cwd                       string   `json:"cwd"`
	AllowWrite                bool     `json:"allowWrite"`
	GitBaseline               *string  `json:"gitBaseline"`
	SessionID                 *string  `json:"sessionId"`
	ResumeCommand             *string  `json:"resumeCommand"` // complete follow-up command, prompt file left as a placeholder
	TakeoverCommand           *string  `json:"takeoverCommand"`
	SessionLockConflict       *string  `json:"sessionLockConflict"`
	StartedAt                 string   `json:"startedAt"`
	EndedAt                   *string  `json:"endedAt"`
	DurationMs                *int64   `json:"durationMs"`
	TimeoutMin                float64  `json:"timeoutMin"`
	DeadlineAt                *string  `json:"deadlineAt"`
	Label                     *string  `json:"label"`
	PromptFile                string   `json:"promptFile"`
	OutDir                    string   `json:"outDir"`
	RawPath                   string   `json:"rawPath"`
	StderrPath                string   `json:"stderrPath"`
	ProgressPath              string   `json:"progressPath"`
	WatchCommand              string   `json:"watchCommand"`
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
	Tokens                    *Tokens  `json:"tokens"`
	CostUSD                   *float64 `json:"costUsd"`
	Error                     *string  `json:"error"`
	ResultKind                string   `json:"resultKind"`
	NextAction                string   `json:"nextAction"`
	RecoveryAction            *string  `json:"recoveryAction"`
	CollectedAt               *string  `json:"collectedAt"`
	ReconciledAt              *string  `json:"reconciledAt,omitempty"`
}

// SchemaVersion for meta.json written by this engine.
const MetaSchemaVersion = 5

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

// ReadMetaFile parses a meta.json. The raw field map lets callers distinguish
// an absent field from an explicit null (collect needs this for collectedAt).
func ReadMetaFile(path string) (*Meta, map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, err
	}
	return &meta, raw, nil
}

// Ptr is a convenience for the schema's many nullable fields.
func Ptr[T any](v T) *T { return &v }

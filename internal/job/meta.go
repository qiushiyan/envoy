package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
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

// TokenCount is one token field the provider reported.
type TokenCount struct {
	Name  string
	Count int64
}

// Counts returns the set fields in display order.
func (t *Tokens) Counts() []TokenCount {
	if t == nil {
		return nil
	}
	var out []TokenCount
	add := func(name string, v *int64) {
		if v != nil {
			out = append(out, TokenCount{name, *v})
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
	Caller *string `json:"caller,omitempty"`
	// InstructionsSha256 is the digest of the standing instructions this turn
	// was sent, archived beside it as instructions.md; absent when the
	// conversation has none, and in older records. It is what tells an archive
	// that was lost from one that never existed.
	InstructionsSha256  *string       `json:"instructionsSha256,omitempty"`
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
	// RunnerLock records that the runner held its claim on this directory
	// (proc.DirLock) for as long as it ran, so whether it is still running
	// is read from that claim rather than from a PID a later process may
	// reuse. Absent in older records, and when the claim could not be taken.
	RunnerLock bool `json:"runnerLock,omitempty"`
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

// MetaSchemaVersion is the schema of a turn record; MetaSchemaInstructed is
// the same record for a conversation with standing instructions. They are the
// only schemas this engine reads or writes. Records from another version are
// refused rather than reinterpreted: a job store holds one engine's records
// at a time.
// Optional additive observations (such as usage) retain this version; absence
// means unavailable, and existing fields keep their semantics. Schema 10
// replaced the worded error, remedy and lock-conflict strings with the
// failure and lock-conflict observations they were worded from.
const MetaSchemaVersion = 10

// MetaSchemaInstructed is MetaSchemaVersion with instructionsSha256 required.
// Its absence is not unavailable but a meaning — the conversation has no
// instructions — so an engine that reads only schema 10 would misread such a
// record: decode it without the digest, write it back that way when a collect
// stamps it, and continue the conversation without its instructions. A version
// of its own makes that engine refuse the record by name instead. A record
// without instructions keeps schema 10, which every engine since reads alike.
const MetaSchemaInstructed = 11

// MetaSchemas are the turn-record schemas this engine reads.
var MetaSchemas = []int{MetaSchemaVersion, MetaSchemaInstructed}

// MetaSchemaFor is the schema a turn record is written in.
func MetaSchemaFor(instructed bool) int {
	if instructed {
		return MetaSchemaInstructed
	}
	return MetaSchemaVersion
}

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

// Stamp is the part of a turn record that naming and pending read: who
// dispatched it, whether it is finished and delivered, and the process that
// supervises it while it runs. No schema change has altered what these fields
// mean — status, collectedAt and runnerPid since the first schema, caller
// and runnerLock since they were first recorded (absent before, which reads
// as no identity and a runner known only by its PID) —
// so a stamp is read from a record of any version. A schema change must
// never take a store's names hostage; everything else in a record of another
// version is still refused by name.
type Stamp struct {
	SchemaVersion int     `json:"schemaVersion"`
	Status        string  `json:"status"`
	Caller        *string `json:"caller"`
	CollectedAt   *string `json:"collectedAt"`
	RunnerPid     int     `json:"runnerPid"`
	RunnerLock    bool    `json:"runnerLock"`
}

// SchemaError is a record another schema version wrote: intact, and refused
// by name wherever its meaning is read, never reinterpreted. Record names the
// file (meta.json or group.json), Version what it carries, and Reads the one
// version this engine reads for it.
type SchemaError struct {
	Record  string
	Version int
	Reads   []int
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("%s is schema %d and this engine reads schema %s only", e.Record, e.Version, SchemaList(e.Reads))
}

// SchemaList spells the versions an engine reads: "10", "10 or 11".
func SchemaList(versions []int) string {
	words := make([]string, len(versions))
	for i, v := range versions {
		words[i] = strconv.Itoa(v)
	}
	return strings.Join(words, " or ")
}

// Record is one turn record as read once (ReadRecord), with what that read
// could and could not establish kept apart: Stamp whenever the record parses,
// from a record of any schema version; Meta only when it is this engine's
// schema, decoded after the version is known, so a field another schema
// shaped differently is refused as that schema (*SchemaError) and never
// misread as damage; and Err whenever Meta is missing — ErrNoRecord, a
// *SchemaError, or damage. A stamp beside an unreadable body is two facts,
// not one verdict: naming reads the stamp, everything else the body. A record
// that does not parse at all has no stamp — damage, never a blank one.
type Record struct {
	Dir   string
	Stamp *Stamp
	Meta  *Meta
	Err   error
}

// ReadRecord reads the turn record in dir once.
func ReadRecord(dir string) Record {
	r := Record{Dir: dir}
	data, err := os.ReadFile(Workspace{Dir: dir}.MetaPath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Err = ErrNoRecord
		return r
	case err != nil:
		r.Err = err
		return r
	}
	var stamp Stamp
	if r.Err = json.Unmarshal(data, &stamp); r.Err != nil {
		return r
	}
	r.Stamp = &stamp
	if !slices.Contains(MetaSchemas, stamp.SchemaVersion) {
		r.Err = &SchemaError{Record: "meta.json", Version: stamp.SchemaVersion, Reads: MetaSchemas}
		return r
	}
	var meta Meta
	if r.Err = json.Unmarshal(data, &meta); r.Err != nil {
		return r
	}
	r.Meta = &meta
	return r
}

// ReadMeta reads the turn record in dir as this engine's schema, ErrNoRecord
// when it has none.
func ReadMeta(dir string) (*Meta, error) {
	r := ReadRecord(dir)
	return r.Meta, r.Err
}

// ReadStamp reads the stamp of the turn record in dir, from a record of any
// schema version, ErrNoRecord when it has none.
func ReadStamp(dir string) (*Stamp, error) {
	r := ReadRecord(dir)
	if r.Stamp == nil {
		return nil, r.Err
	}
	return r.Stamp, nil
}

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

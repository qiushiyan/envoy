package job

// Failure is why a turn did not deliver, as observed: the cause, and the
// provider's or the system's own words where the cause carries some. It holds
// no sentence and no command. internal/prose words it whenever a record is
// read, so a stored job always reads in the current engine's vocabulary and
// nothing persisted drifts with a wording change.
//
// The facts a cause needs and the record already holds — the provider, the
// exit code or signal, the cap, the launcher — are read from the record, not
// copied here.
type Failure struct {
	Cause string `json:"cause"`
	// Message is the provider's own failure verdict, or the error that
	// stopped a spawn; absent when the provider gave no words.
	Message *string `json:"message,omitempty"`
	// Code is the provider's own name for its verdict, where it gives one
	// (claude's result subtype).
	Code *string `json:"code,omitempty"`
	// StderrTail is the last few non-empty lines of the command's stderr, for
	// an exit that left no result.
	StderrTail []string `json:"stderrTail,omitempty"`
	// LastErrorEvent is the provider's last transient error event, for an
	// exit that left no result.
	LastErrorEvent *string `json:"lastErrorEvent,omitempty"`
	// Stream is what the run had observed of the provider's stream at the
	// instant the cap fired, before any teardown output.
	Stream *StreamSample `json:"stream,omitempty"`
}

// Failure causes. Each names an observation, not a verdict about the work.
const (
	CauseSpawnFailed           = "spawn_failed"            // the command never launched
	CauseProviderVerdict       = "provider_verdict"        // the provider reported a failure of its own
	CauseBudgetCap             = "budget_cap"              // the provider stopped at the spend cap
	CauseExitedAfterResponse   = "exited_after_response"   // a non-zero exit after a response arrived
	CauseExitedWithoutResult   = "exited_without_result"   // an exit that left no usable result
	CauseExitedWithoutEnvelope = "exited_without_envelope" // an exit that left no parseable result envelope
	CauseForeignSignal         = "foreign_signal"          // killed by a signal envoy did not send
	CauseTimeout               = "timeout"                 // the wall-clock cap ended the turn
	CauseInterrupted           = "interrupted"             // a signal envoy received stopped the turn
	CauseSessionConflict       = "session_conflict"        // the provider reported a session another turn holds
	CauseAbandoned             = "abandoned"               // the runner died without publishing; set by collect
)

// StreamSample is what a run had observed of the provider's stream at one
// instant: how much arrived, and how long it had been quiet. Bytes and events
// are separate observations — a provider can write output that yields no
// event envoy can parse.
type StreamSample struct {
	Events    int64  `json:"events"`
	Bytes     int64  `json:"bytes"`
	LastEvent string `json:"lastEvent,omitempty"`
	QuietMs   int64  `json:"quietMs"`
}

// LockConflict is a session lock a turn could not take: the session, the lock
// file, and what that file said about the turn holding it — or, for a lock
// failure that was not a conflict, the error.
type LockConflict struct {
	SessionID string `json:"sessionId"`
	LockPath  string `json:"lockPath,omitempty"`
	// Holder is the lock file's record of the turn holding the session; nil
	// when the file could not be read.
	Holder *LockHolder `json:"holder,omitempty"`
	// HolderLive says the holder's runner was provably alive. A holder that is
	// not is still never reclaimed: its provider may outlive it.
	HolderLive bool `json:"holderLive"`
	// Error is a lock failure that was not a conflict.
	Error *string `json:"error,omitempty"`
}

// LockHolder is the turn a session lock names.
type LockHolder struct {
	Pid       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	Dir       string `json:"dir"`
}

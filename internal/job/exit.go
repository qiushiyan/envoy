package job

import "slices"

// Exit codes are part of the caller interface (CLAUDE.md § The Output
// Contract): every surface that ends a dispatch, a wait or a collect exits
// with one of these.
const (
	ExitOK          = 0
	ExitFailed      = 1
	ExitInfra       = 2
	ExitUsage       = 3
	ExitTimeout     = 4
	ExitInterrupted = 5
)

// ExitCodeFor maps a terminal status to the process exit code.
func ExitCodeFor(status string) int {
	switch status {
	case StatusOK:
		return ExitOK
	case StatusFailed:
		return ExitFailed
	case StatusTimeout:
		return ExitTimeout
	case StatusInterrupted:
		return ExitInterrupted
	default:
		return ExitInfra
	}
}

// ExitPartial reports a fan-out where some members returned a result and
// others did not. It exists because neither 0 nor any failure code licenses
// the right move there: the results that landed are usable, and only the
// members that failed need a decision. Only a fan-out can exit this way — a
// single turn's codes are unchanged.
const ExitPartial = 6

// groupExitPrecedence orders the codes for a fan-out where no member returned
// a result, worst first. A dispatch-side cause outranks a provider-side one:
// rejected flags or a locked session, then an envoy/environment failure, then
// a stopped turn, the cap, and last the provider's own verdict. The earlier
// ones are fixed by changing the command, the later ones by waiting or
// changing the prompt.
var groupExitPrecedence = []int{ExitUsage, ExitInfra, ExitInterrupted, ExitTimeout, ExitFailed}

// ExitCodeForGroup aggregates the members' exit codes into the fan-out's own.
func ExitCodeForGroup(codes []int) int {
	if len(codes) == 0 {
		return ExitInfra
	}
	ok := 0
	for _, c := range codes {
		if c == ExitOK {
			ok++
		}
	}
	switch {
	case ok == len(codes):
		return ExitOK
	case ok > 0:
		return ExitPartial
	}
	for _, want := range groupExitPrecedence {
		if slices.Contains(codes, want) {
			return want
		}
	}
	return ExitInfra
}

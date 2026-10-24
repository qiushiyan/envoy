// Package prose holds every sentence envoy addresses to its caller.
//
// The caller is usually an AI agent reading stdout, so this text is a prompt
// surface: it must say what happened, what that rules in or out, and the one
// action to take next — and it may only prescribe what the engine actually
// observed. Centralizing it here keeps a single wording for each situation
// (the runner, the drivers, and collect all describe the same few outcomes)
// and makes the whole vocabulary reviewable in one package, one file per
// family: the commands it hands over, why a turn failed, what to do next,
// names, rosters, fan-outs, the pending index, the waiter, and the diagnostic
// tier.
//
// Everyone else contributes observations — typed fields, causes, the
// provider's own words — and the records hold only those, so a stored job
// is always worded in the vocabulary of the engine reading it.
package prose

import (
	"fmt"
	"strings"
	"time"
)

// The tokens sentences, commands and progress lines are built from — a quoted
// path, a duration, a cap — are spelled here too, so every surface that
// prints one spells it the same way.

// ShellQuote single-quotes a value for copy-pasteable command lines.
func ShellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// FormatDuration renders elapsed time the way progress lines and recovery
// prose expect: seconds under 90s, then minutes, then h+m.
func FormatDuration(d time.Duration) string {
	ms := d.Milliseconds()
	if ms < 90_000 {
		s := max(ms/1000, 0)
		return fmt.Sprintf("%ds", s)
	}
	totalMinutes := ms / 60_000
	hours := totalMinutes / 60
	minutes := totalMinutes % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", totalMinutes)
}

// HardCap renders a --timeout-min value for coordinate lines: "off" for 0,
// otherwise the minute count with trailing m (fractions kept as given).
func HardCap(timeoutMin float64) string {
	if timeoutMin == 0 {
		return "off"
	}
	if timeoutMin == float64(int64(timeoutMin)) {
		return fmt.Sprintf("%dm", int64(timeoutMin))
	}
	return fmt.Sprintf("%gm", timeoutMin)
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// capitalize spells a provider name the way a sentence opens with it.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Package text holds the small formatting helpers shared by every layer that
// prints for the calling agent.
package text

import (
	"fmt"
	"strings"
	"time"
)

// ShellQuote single-quotes a value for copy-pasteable command lines.
func ShellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// FormatDuration renders elapsed time the way progress lines and recovery
// prose expect: seconds under 90s, then minutes, then h+m.
func FormatDuration(d time.Duration) string {
	ms := d.Milliseconds()
	if ms < 90_000 {
		s := ms / 1000
		if s < 0 {
			s = 0
		}
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

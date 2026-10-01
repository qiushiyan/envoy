package prose

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/text"
)

// Launcher renders the recorded invocation, never the collector's environment.
func Launcher(m *job.Meta) string {
	if !m.UsesLauncher() {
		return ""
	}
	return fmt.Sprintf("launcher: %s=%s", launcherEnv(m.Provider), text.ShellQuote(strings.Join(m.CommandPrefix, " ")))
}

func launcherEnv(provider string) string { return "ENVOY_" + strings.ToUpper(provider) + "_CMD" }

// Setting renders a model or effort as the turn was dispatched with it. An
// omitted one is the provider's own configuration, reported as such — never a
// guess at what that configuration resolves to.
func Setting(v string) string {
	if v == "" {
		return "(provider default)"
	}
	return v
}

// ModelSetting renders the requested model with the one the provider reported
// running, when that report spells something the request did not. A
// difference is the provider's own alias resolution as often as anything
// else, so this shows both and concludes nothing.
func ModelSetting(requested, reported string) string {
	switch {
	case reported == "" || reported == requested:
		return Setting(requested)
	case requested == "":
		return fmt.Sprintf("(provider default, ran %s)", reported)
	default:
		return fmt.Sprintf("%s (ran %s)", requested, reported)
	}
}

// ProviderStream words the tally of a provider's own connection-error
// events, as a diagnostic line beside the block — never inside the timeout
// gloss, because status and recovery follow from terminal and prompt evidence
// alone. It reports and stops: "connection-error events" is what the provider
// said, and "offline" would be the engine's inference. nil (the driver does
// not observe them, or an older engine never counted) prints nothing; zero is
// a real observation and prints.
func ProviderStream(ce *job.ConnectionErrors) string {
	if ce == nil {
		return ""
	}
	if ce.Count == 0 {
		return "provider stream: no connection-error events recognized by this engine version"
	}
	line := fmt.Sprintf("provider stream: %d recognized connection-error event%s", ce.Count, plural(ce.Count))
	if ce.FirstAt != nil && ce.LastAt != nil {
		line += fmt.Sprintf("; first observed %s, last observed %s", clockOf(*ce.FirstAt), clockOf(*ce.LastAt))
	}
	return line
}

// ContextUsage describes the latest sampled request input. It belongs in the
// diagnostic tier; incompleteness is telemetry, never a recovery prescription.
func ContextUsage(u *job.Usage) string {
	if u == nil {
		return ""
	}
	line := "context: no measurement yet"
	if u.LatestContextTokens != nil {
		line = "context: " + compactTokens(*u.LatestContextTokens)
		if u.ContextWindowTokens != nil && *u.ContextWindowTokens > 0 {
			line += fmt.Sprintf(" of %s (%.0f%%)", compactTokens(*u.ContextWindowTokens),
				100*float64(*u.LatestContextTokens)/float64(*u.ContextWindowTokens))
		} else {
			line += ", window unknown"
		}
		if u.PeakContextTokens != nil {
			line += ", peak " + compactTokens(*u.PeakContextTokens)
		}
		line += fmt.Sprintf(", %d response%s", u.Responses, plural(u.Responses))
	}
	var issues []string
	for _, issue := range u.Issues {
		var detail string
		switch issue {
		case "missing_terminal":
			detail = "no terminal record"
		case "missing_window":
			detail = "window unknown"
		case "missing_context_sample":
			detail = "no context sample"
		case "missing_model":
			detail = "model attribution missing"
		case "model_mismatch":
			detail = "unexpected model"
			if u.UnexpectedModel != nil {
				detail += fmt.Sprintf(" %q", *u.UnexpectedModel)
			}
		case "missing_message_id":
			detail = "response ID missing"
		case "missing_input_usage":
			detail = "response input usage missing or invalid"
		case "missing_terminal_usage":
			detail = "terminal usage missing or invalid"
		case "terminal_usage_incomplete":
			detail = "terminal usage incomplete"
		case "terminal_usage_mismatch":
			detail = "terminal usage disagrees with observed responses"
		default:
			detail = "measurement evidence incomplete"
		}
		issues = append(issues, detail)
	}
	if len(issues) > 0 {
		line += "; incomplete (" + strings.Join(issues, "; ") + ")"
	}
	return line
}

// Tokens renders the provider-reported token counts, "n/a" when it reported
// none.
func Tokens(t *job.Tokens) string {
	counts := t.Counts()
	if len(counts) == 0 {
		return "n/a"
	}
	parts := make([]string, len(counts))
	for i, c := range counts {
		parts[i] = c.Name + " " + groupThousands(c.Count)
	}
	return strings.Join(parts, " · ")
}

func groupThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func compactTokens(n int64) string {
	unit, suffix := float64(1), ""
	switch {
	case n >= 1000000:
		unit, suffix = 1000000, "M"
	case n >= 1000:
		unit, suffix = 1000, "k"
	default:
		return fmt.Sprint(n)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/unit), ".0") + suffix
}

// clockOf renders an RFC 3339 instant as HH:MMZ, the resolution a caller
// comparing it to a cap or a commute needs; an unparseable stamp prints as is.
func clockOf(stamp string) string {
	if t, err := time.Parse(time.RFC3339, stamp); err == nil {
		return t.UTC().Format("15:04Z")
	}
	return stamp
}

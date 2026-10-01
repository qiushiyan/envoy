// Package prose holds every sentence envoy addresses to its caller.
//
// The caller is usually an AI agent reading stdout, so this text is a prompt
// surface: it must say what happened, what that rules in or out, and the one
// action to take next — and it may only prescribe what the engine actually
// observed. Centralizing it here keeps a single wording for each situation
// (the runner, the drivers, and collect all describe the same few outcomes)
// and makes the whole vocabulary reviewable in one package, one file per
// family: the commands it hands over, why a turn failed, what to do next,
// names, rosters, fan-outs, and the diagnostic tier.
//
// Everyone else contributes observations — typed fields, causes, the
// provider's own words — and the records hold only those, so a stored job
// is always worded in the vocabulary of the engine reading it.
package prose

import "strings"

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

package runner

import (
	"os"
	"slices"
	"testing"
)

// A launcher prefix is argv, not shell code: words split on whitespace and are
// taken literally. Unset, empty or whitespace-only keeps the bare provider.
func TestCommandPrefixUsesLiteralWhitespaceSeparatedWords(t *testing.T) {
	for _, c := range []struct {
		name, value string
		unset       bool
		want        []string
	}{
		{"unset", "", true, []string{"codex"}},
		{"empty", "", false, []string{"codex"}},
		{"whitespace only", " \t\n", false, []string{"codex"}},
		{"words taken literally", " \tlauncher  --vendor\ncodex -- '$HOME' *.txt ; ", false,
			[]string{"launcher", "--vendor", "codex", "--", "'$HOME'", "*.txt", ";"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ENVOY_CODEX_CMD", c.value)
			if c.unset {
				os.Unsetenv("ENVOY_CODEX_CMD")
			}
			if got := commandPrefix("codex"); !slices.Equal(got, c.want) {
				t.Fatalf("prefix = %q, want %q", got, c.want)
			}
		})
	}
}

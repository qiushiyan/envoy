package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDefaultCommandPreservesArgvAndEnv(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, value := range []string{"unset", "", " \t\n"} {
			t.Run(provider+"/"+value, func(t *testing.T) {
				key := "ENVOY_CLAUDE_CMD"
				if provider == "codex" {
					key = "ENVOY_CODEX_CMD"
				}
				t.Setenv(key, value)
				if value == "unset" {
					os.Unsetenv(key)
				}
				bin := t.TempDir()
				if err := os.WriteFile(filepath.Join(bin, provider), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
				// The dispatcher's session identity is the one thing the child
				// does not inherit: it is a session of its own.
				t.Setenv("ENVOY_CALLER", "dispatcher")
				t.Setenv("CLAUDE_CODE_SESSION_ID", "dispatcher-session")
				argv := []string{"arg with spaces", "'literal'", "$(literal)"}
				extraEnv := []string{"ENVOY_TEST_EXTRA=value"}
				prefix := commandPrefix(provider)
				child, err := spawn(prefix[0], slices.Concat(prefix[1:], argv), bin, extraEnv)
				if err != nil {
					t.Fatal(err)
				}
				defer child.stdin.Close()
				defer child.stdout.Close()
				defer child.stderr.Close()
				child.wait()
				old := exec.Command(provider, argv...)
				old.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
					return strings.HasPrefix(kv, "ENVOY_CALLER=") || strings.HasPrefix(kv, "CLAUDE_CODE_SESSION_ID=")
				}), extraEnv...)
				if slices.ContainsFunc(child.cmd.Env, func(kv string) bool { return strings.Contains(kv, "dispatcher") }) {
					t.Fatal("the provider child inherited the dispatcher's session identity")
				}
				if !slices.Equal(prefix, []string{provider}) || child.cmd.Path != old.Path ||
					!slices.Equal(child.cmd.Args, old.Args) || !slices.Equal(child.cmd.Env, old.Env) {
					t.Fatal("unconfigured command changed executable, argv or environment")
				}
			})
		}
	}
}

func TestCommandPrefixUsesLiteralWhitespaceSeparatedWords(t *testing.T) {
	t.Setenv("ENVOY_CODEX_CMD", " \tlauncher  --vendor\ncodex -- '$HOME' *.txt ; ")
	want := []string{"launcher", "--vendor", "codex", "--", "'$HOME'", "*.txt", ";"}
	if got := commandPrefix("codex"); !slices.Equal(got, want) {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
}

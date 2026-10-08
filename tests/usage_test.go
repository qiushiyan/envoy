package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClaudeUsageLiveAndCollected(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "continue")
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "usage-capture").
		set("ENVOY_HEARTBEAT_MS", "60000").set("ENVOY_FAKE_USAGE_GATE", gate)
	outDir := filepath.Join(dir, "job")
	prompt := writePrompt(t, dir)
	cmd := exec.Command(binPath, runArgs(prompt, outDir, "--with", "claude", "--timeout-min", "1")...)
	cmd.Env = e.build()
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			killDispatch(cmd, outDir)
			cmd.Wait()
		}
	})
	for _, step := range []struct {
		responses, gate int
		context         float64
	}{{1, 1, 18889}, {2, 3, 19323}} {
		deadline := time.Now().Add(5 * time.Second)
		var observed bool
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(filepath.Join(outDir, "meta.json"))
			if err == nil {
				var meta map[string]any
				if err := json.Unmarshal(data, &meta); err != nil {
					t.Fatalf("live reader saw a torn metadata write: %v", err)
				}
				if u, ok := meta["usage"].(map[string]any); ok && u["responses"] == float64(step.responses) {
					if meta["status"] != "running" || u["state"] != "live" || u["final"] != false || u["outputTokens"] != nil || u["contextWindowTokens"] != nil || u["latestContextTokens"] != step.context || meta["tokens"] != nil {
						t.Fatalf("invalid live observation: %v", meta)
					}
					observed = true
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !observed {
			t.Fatalf("response %d did not reach meta before the heartbeat", step.responses)
		}
		status := runEnvoy(t, e, "collect", "--status-only", outDir)
		mustContain(t, "live context", status.stdout, "context: ", "window unknown")
		if readMeta(t, outDir)["collectedAt"] != nil {
			t.Fatal("live status stamped collection")
		}
		if err := os.WriteFile(gate+"-"+strconv.Itoa(step.gate), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("run failed: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	meta := readMeta(t, outDir)
	u := meta["usage"].(map[string]any)
	if meta["schemaVersion"] != float64(10) || u["state"] != "settled" || u["final"] != true || u["responses"] != float64(3) || u["outputTokens"] != float64(338) || u["latestContextTokens"] != float64(19454) {
		t.Fatalf("settled usage = %v", meta)
	}
	tokens := meta["tokens"].(map[string]any)
	if tokens["input"] != float64(66) || tokens["output"] != float64(338) {
		t.Fatalf("existing tokens writer changed: %v", tokens)
	}
	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	mustContain(t, "settled context", status.stdout, "context: 19.5k of 1M (2%), peak 19.5k, 3 responses")
	if readMeta(t, outDir)["collectedAt"] != nil {
		t.Fatal("settled status stamped collection")
	}
	collected := runEnvoy(t, e, "collect", outDir)
	mustNotContain(t, "delivered result", collected.stdout, "context: ")
	mustContain(t, "delivered result", collected.stdout, "telemetry capture")
	if readMeta(t, outDir)["collectedAt"] == nil {
		t.Fatal("successful result delivery did not stamp collection")
	}
	// A continued turn begins its own counters even with the same session.
	delete(e.extra, "ENVOY_FAKE_USAGE_GATE")
	resumedDir := filepath.Join(dir, "resumed")
	resumed := runEnvoy(t, e, runArgs(prompt, resumedDir, "--with", "@"+outDir, "--timeout-min", "1")...)
	if resumed.code != 0 || readMeta(t, resumedDir)["usage"].(map[string]any)["responses"] != float64(3) {
		t.Fatalf("continued turn failed or inherited counts: %s\n%s", resumed.stdout, resumed.stderr)
	}
}

func TestClaudeUsageIncompleteEndings(t *testing.T) {
	for _, tc := range []struct {
		ending, issue string
		code          int
	}{
		{"missing", "missing_terminal_usage", 0},
		{"budget", "terminal_usage_incomplete", 1},
		{"crash", "terminal_usage_incomplete", 1},
		{"eof", "missing_terminal", 2},
		{"hang", "missing_terminal", 4},
	} {
		t.Run(tc.ending, func(t *testing.T) {
			e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "usage-capture").set("ENVOY_FAKE_USAGE_END", tc.ending)
			outDir := filepath.Join(t.TempDir(), "job")
			prompt := writePrompt(t, t.TempDir())
			res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude", "--timeout-min", "0.02")...)
			if res.code != tc.code {
				t.Fatalf("exit = %d, want %d\n%s\n%s", res.code, tc.code, res.stdout, res.stderr)
			}
			u := readMeta(t, outDir)["usage"].(map[string]any)
			if u["state"] != "incomplete" || u["final"] != true || u["responses"] != float64(3) || u["inputTokens"] != float64(66) || u["latestContextTokens"] != float64(19454) || u["outputTokens"] != nil {
				t.Fatalf("incomplete end erased or invented usage: %v", u)
			}
			found := false
			for _, issue := range u["issues"].([]any) {
				found = found || issue == tc.issue
			}
			if !found {
				t.Fatalf("missing issue %s in %v", tc.issue, u)
			}
			status := runEnvoy(t, e, "collect", "--status-only", outDir)
			mustContain(t, "incomplete context", status.stdout, "context: ", "; incomplete (")
		})
	}
}

func TestCodexHasNoClaudeUsage(t *testing.T) {
	e := newEnv(t)
	outDir := filepath.Join(t.TempDir(), "job")
	res := runEnvoy(t, e, runArgs(writePrompt(t, t.TempDir()), outDir, "--with", "codex")...)
	if res.code != 0 {
		t.Fatalf("run failed: %s", res.stderr)
	}
	if _, present := readMeta(t, outDir)["usage"]; present {
		t.Fatal("Codex emitted unsupported usage")
	}
	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	mustNotContain(t, "Codex context", status.stdout, "context: ")
}

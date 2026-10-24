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
	if u["state"] != "settled" || u["final"] != true || u["responses"] != float64(3) || u["outputTokens"] != float64(338) || u["latestContextTokens"] != float64(19454) {
		t.Fatalf("settled usage = %v", meta)
	}
	tokens := meta["tokens"].(map[string]any)
	if tokens["input"] != float64(66) || tokens["output"] != float64(338) {
		t.Fatalf("existing tokens writer changed: %v", tokens)
	}
	status := runEnvoy(t, e, "collect", "--status-only", outDir)
	mustContain(t, "settled context", status.stdout, "context: 19.5k", "of 1M", "3 responses")
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

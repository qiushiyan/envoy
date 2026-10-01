package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Records the launcher's actual argv/environment without reading stdin, then
// either replaces itself or keeps an extra parent in the supervised group.
func launcherEnv(t *testing.T, e *env, wrapper bool) string {
	t.Helper()
	dir := t.TempDir()
	end := "exec \"$provider\" \"$@\"\n"
	if wrapper {
		end = "exec 3<&0\n\"$provider\" \"$@\" <&3 &\nexec 3<&-\nwait $!\n"
	}
	script := `#!/bin/sh
node -e 'require("fs").writeFileSync(process.env.ENVOY_LAUNCH_RECORD + "-" + process.argv[1], JSON.stringify({argv:process.argv.slice(1),env:process.env,pid:process.ppid}))' "$@"
provider="$1"
shift
[ "$1" = -- ] || exit 64
shift
printf 'launcher neutralized an inherited variable\n' >&2
` + end
	for _, name := range []string{"stub-launcher", "next-launcher"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	record := filepath.Join(t.TempDir(), "launch")
	e.set("PATH", dir+":"+fakeBin+":"+os.Getenv("PATH")).set("ENVOY_LAUNCH_RECORD", record)
	return record
}

type invocation struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
	Pid  int               `json:"pid"`
}

func readInvocation(t *testing.T, path string) invocation {
	t.Helper()
	var v invocation
	if err := json.Unmarshal([]byte(readFile(t, path)), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func stringsFromMeta(t *testing.T, m map[string]any, key string) []string {
	t.Helper()
	raw, ok := m[key].([]any)
	if !ok {
		t.Fatalf("missing %s: %v", key, m[key])
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i] = v.(string)
	}
	return out
}

func TestLauncherFreshAndResume(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_TEST_AMBIENT", "inherited value")
			record := launcherEnv(t, e, false)
			captured := filepath.Join(t.TempDir(), "provider.json")
			received := filepath.Join(t.TempDir(), "received-prompt")
			e.set("ENVOY_FAKE_INVOCATION_FILE", captured).set("ENVOY_FAKE_PROMPT_FILE", received)
			prompt := writePrompt(t, t.TempDir())
			var source string
			for _, launcher := range []string{"stub-launcher", "next-launcher"} {
				e.set("ENVOY_"+strings.ToUpper(provider)+"_CMD", launcher+" "+provider+" --")
				voice := provider + ":model with spaces:high"
				if source != "" {
					voice = "@" + source
				}
				out := filepath.Join(t.TempDir(), "job")
				res := runEnvoy(t, e, runArgs(prompt, out, "--with", voice)...)
				if res.code != 0 {
					t.Fatalf("exit %d: %s\n%s", res.code, res.stdout, res.stderr)
				}
				m := readMeta(t, out)
				mustContain(t, "dispatch launcher", res.stdout, "launcher: ENVOY_"+strings.ToUpper(provider)+"_CMD=", launcher+" "+provider+" --")
				prefix := []string{launcher, provider, "--"}
				if got := stringsFromMeta(t, m, "commandPrefix"); !reflect.DeepEqual(got, prefix) {
					t.Fatalf("prefix %q", got)
				}
				launched := readInvocation(t, record+"-"+provider)
				child := readInvocation(t, captured)
				native := stringsFromMeta(t, m, "providerArgv")[1:]
				if !reflect.DeepEqual(launched.Argv, append(prefix[1:], native...)) || !reflect.DeepEqual(child.Argv, native) {
					t.Fatalf("argv changed: launcher %q, provider %q, native %q", launched.Argv, child.Argv, native)
				}
				if launched.Pid != child.Pid || int(m["providerPid"].(float64)) != child.Pid {
					t.Fatal("exec launcher did not retain the supervised PID")
				}
				for _, observed := range []invocation{launched, child} {
					if observed.Env["ENVOY_TEST_AMBIENT"] != "inherited value" {
						t.Fatal("lost inherited environment")
					}
					if provider == "claude" && observed.Env["API_FORCE_IDLE_TIMEOUT"] != "1" {
						t.Fatal("lost driver environment")
					}
				}
				if readFile(t, received) != readFile(t, prompt) {
					t.Fatal("stdin changed")
				}
				if readFile(t, filepath.Join(out, "result.md")) != "fake provider result" {
					t.Fatal("result not delivered")
				}
				mustContain(t, "stderr.log", readFile(t, filepath.Join(out, "stderr.log")), "launcher neutralized")
				col := runEnvoy(t, e, "collect", out)
				mustNotContain(t, "delivered result", col.stdout, "launcher:")
				diag := runEnvoy(t, e, "collect", "--status-only", out)
				mustContain(t, "launcher diagnostic", diag.stdout, "launcher: ENVOY_"+strings.ToUpper(provider)+"_CMD=", launcher+" "+provider+" --")
				if source != "" {
					if m["resumedFrom"] != source {
						t.Fatal("lost continuation lineage")
					}
					mustContain(t, "resume argv", strings.Join(child.Argv, " "), "resume", readMeta(t, source)["sessionId"].(string))
				}
				source = out
			}
			// A different launcher cannot bypass a held session's preflight lock.
			lockDir := filepath.Join(e.home, ".local", "state", "envoy", "locks")
			lockPath := filepath.Join(lockDir, readMeta(t, source)["sessionId"].(string)+".lock")
			if err := os.WriteFile(lockPath, []byte(fmt.Sprintf(`{"pid":%d,"outDir":%q}`, os.Getpid(), source)), 0o644); err != nil {
				t.Fatal(err)
			}
			os.Remove(record + "-" + provider)
			e.set("ENVOY_"+strings.ToUpper(provider)+"_CMD", "stub-launcher "+provider+" --")
			r := runEnvoy(t, e, runArgs(prompt, filepath.Join(t.TempDir(), "blocked"), "--with", "@"+source)...)
			if r.code != 3 {
				t.Fatalf("held session exit %d: %s", r.code, r.stderr)
			}
			if _, err := os.Stat(record + "-" + provider); !os.IsNotExist(err) {
				t.Fatal("launcher ran despite preflight session lock")
			}
		})
	}
}

func TestLauncherFailureNeverFallsBack(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, missing := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/missing=%v", provider, missing), func(t *testing.T) {
				e := newEnv(t)
				launcherEnv(t, e, false)
				name := "missing-launcher-for-envoy-test"
				if !missing {
					name = filepath.Join(t.TempDir(), "refusing-launcher")
					if err := os.WriteFile(name, []byte("#!/bin/sh\nprintf 'launcher: corrupt account selection\\n' >&2\nexit 23\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				capture := filepath.Join(t.TempDir(), "provider.json")
				e.set("ENVOY_"+strings.ToUpper(provider)+"_CMD", name).set("ENVOY_FAKE_INVOCATION_FILE", capture)
				out := filepath.Join(t.TempDir(), "job")
				r := runEnvoy(t, e, runArgs(writePrompt(t, t.TempDir()), out, "--with", provider)...)
				if r.code != 2 {
					t.Fatalf("exit %d: %s\n%s", r.code, r.stdout, r.stderr)
				}
				m := readMeta(t, out)
				if m["status"] != "infra" {
					t.Fatalf("status %v", m["status"])
				}
				if got := stringsFromMeta(t, m, "commandPrefix"); !reflect.DeepEqual(got, []string{name}) {
					t.Fatalf("prefix %q", got)
				}
				if _, err := os.Stat(capture); !os.IsNotExist(err) {
					t.Fatal("provider started despite launcher failure")
				}
				key := "ENVOY_" + strings.ToUpper(provider) + "_CMD"
				e.set(key, "different-launcher-at-collection")
				col := runEnvoy(t, e, "collect", out)
				mustContain(t, "recovery", col.stdout, key, "Each follow-up reads the current value of "+key+".")
				mustNotContain(t, "recovery", col.stdout, "the provider CLI itself is the problem")
				mustContain(t, "launcher diagnostic", col.stdout, "launcher: "+key+"=", name)
				if missing {
					mustContain(t, "recovery", col.stdout, "Fix "+key+" or make its executable available first.")
					mustContain(t, "error", failureText(t, out), name)
					if m["promptState"] != "not_started" || m["providerPid"] != nil {
						t.Fatal("spawn failure lost evidence")
					}
				} else {
					mustContain(t, "error", failureText(t, out), "corrupt account selection")
					mustContain(t, "error", failureText(t, out), fmt.Sprintf("Command %q exited with code 23", name))
					if readFile(t, filepath.Join(out, "stderr.log")) != "launcher: corrupt account selection\n" {
						t.Fatal("stderr changed")
					}
					if m["childExitCode"] != float64(23) || m["promptState"] != "unknown" {
						t.Fatal("launcher exit is not proof of prompt state")
					}
				}
			})
		}
	}
}

func TestFanUsesEachProviderLauncher(t *testing.T) {
	e := newEnv(t)
	record := launcherEnv(t, e, false)
	e.set("ENVOY_CODEX_CMD", "stub-launcher codex --").set("ENVOY_CLAUDE_CMD", "next-launcher claude --")
	out := filepath.Join(t.TempDir(), "group")
	r := runEnvoy(t, e, runArgs(writePrompt(t, t.TempDir()), out, "--with", "codex", "--with", "claude:opus")...)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, c := range []struct{ member, provider, launcher string }{{"codex", "codex", "stub-launcher"}, {"claude-opus", "claude", "next-launcher"}} {
		m := readMeta(t, filepath.Join(out, c.member))
		if !reflect.DeepEqual(stringsFromMeta(t, m, "commandPrefix"), []string{c.launcher, c.provider, "--"}) {
			t.Fatal("wrong member launcher")
		}
		readInvocation(t, record+"-"+c.provider)
		if m["status"] != "ok" {
			t.Fatal("member did not deliver")
		}
	}
}

func TestLauncherSupervision(t *testing.T) {
	for _, wrapper := range []bool{false, true} {
		for _, interrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("wrapper=%v/interrupt=%v", wrapper, interrupt), func(t *testing.T) {
				e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "hang-with-stubborn-grandchild-only")
				record := launcherEnv(t, e, wrapper)
				e.set("ENVOY_CODEX_CMD", "stub-launcher codex --")
				pidFile, readyFile, capture := filepath.Join(t.TempDir(), "grandchild.pid"), filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "provider.json")
				e.set("ENVOY_FAKE_GRANDCHILD_PID_FILE", pidFile).set("ENVOY_FAKE_GRANDCHILD_READY_FILE", readyFile).set("ENVOY_FAKE_INVOCATION_FILE", capture)
				out := filepath.Join(t.TempDir(), "job")
				cap := "0.05"
				if interrupt {
					cap = "5"
				}
				cmd := exec.Command(binPath, runArgs(writePrompt(t, t.TempDir()), out, "--with", "codex", "--timeout-min", cap)...)
				cmd.Env = e.build()
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				t.Cleanup(func() {
					if data, err := os.ReadFile(filepath.Join(out, "meta.json")); err == nil {
						var m map[string]any
						if json.Unmarshal(data, &m) == nil {
							if pid, ok := m["providerPid"].(float64); ok {
								syscall.Kill(-int(pid), syscall.SIGKILL)
							}
						}
					}
					cmd.Process.Kill()
				})
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(readyFile); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("grandchild never ready")
					}
					time.Sleep(20 * time.Millisecond)
				}
				if interrupt {
					if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case err := <-done:
					want := 4
					if interrupt {
						want = 5
					}
					ee, ok := err.(*exec.ExitError)
					if !ok || ee.ExitCode() != want {
						t.Fatalf("exit %v, want %d", err, want)
					}
				case <-time.After(15 * time.Second):
					t.Fatal("launcher tree stalled cleanup")
				}
				var grandchild int
				fmt.Sscanf(readFile(t, pidFile), "%d", &grandchild)
				pids := []int{readInvocation(t, record+"-codex").Pid, readInvocation(t, capture).Pid, grandchild}
				for _, pid := range pids {
					if pid <= 0 {
						t.Fatal("invalid recorded PID")
					}
					deadline := time.Now().Add(3 * time.Second)
					for syscall.Kill(pid, 0) != syscall.ESRCH {
						if time.Now().After(deadline) {
							t.Fatalf("process %d survived tree cleanup", pid)
						}
						time.Sleep(25 * time.Millisecond)
					}
				}
			})
		}
	}
}

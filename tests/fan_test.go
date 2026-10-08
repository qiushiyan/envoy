package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Fan-outs: several turns supervised as one job, collected as one block,
// continued as one round, and recovered per member.

func readGroup(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "group.json"))
	if err != nil {
		t.Fatalf("group.json: %v", err)
	}
	var g map[string]any
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("group.json parse: %v\n%s", err, data)
	}
	return g
}

// The fan-out contract: one command, one completion, one directory — and
// underneath it, members that are ordinary turns in every respect.
func TestFanDispatchesEveryMemberAsAnOrdinaryTurn(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "group")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"job: "+outDir,
		"fan-out: 2 turns · hard cap 5m each",
		"member codex: model (provider default) · effort (provider default) · dir "+filepath.Join(outDir, "codex"),
		"member claude-opus: model opus · effort (provider default) · dir "+filepath.Join(outDir, "claude-opus"),
		"next: let this command run to completion — it exits once every member is done",
		"status: ok — all 2 turns returned a result",
		"member codex: ok · result ",
		"member claude-opus: ok · result ",
		"next: Collect the fan-out: envoy collect ",
	)
	// A member's stdout block would interleave with its siblings', so the
	// single-turn lines stay in each member's own meta.json.
	if strings.Contains(res.stdout, "next: Collect and verify this job") {
		t.Fatalf("member turn blocks must not reach the fan-out's stdout:\n%s", res.stdout)
	}

	for name, wantProvider := range map[string]string{"codex": "codex", "claude-opus": "claude"} {
		memberDir := filepath.Join(outDir, name)
		meta := readMeta(t, memberDir)
		if meta["status"] != "ok" || meta["provider"] != wantProvider || meta["promptState"] != "accepted" {
			t.Fatalf("%s meta = status %v provider %v prompt %v", name, meta["status"], meta["provider"], meta["promptState"])
		}
		if meta["allowWrite"] != false {
			t.Fatalf("%s: a fan-out member must be read-only", name)
		}
		if got := readFile(t, filepath.Join(memberDir, "result.md")); got != "fake provider result" {
			t.Fatalf("%s result.md = %q", name, got)
		}
		if got := readFile(t, filepath.Join(memberDir, "prompt.md")); got != "fake prompt body\n" {
			t.Fatalf("%s prompt.md = %q", name, got)
		}
	}
	// The prompt is a member's own record: the fan-out directory holds the
	// roster and nothing a member already archives.
	if _, err := os.Stat(filepath.Join(outDir, "prompt.md")); !os.IsNotExist(err) {
		t.Fatal("a fan-out directory must hold no prompt.md of its own")
	}

	var group map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(outDir, "group.json"))), &group); err != nil {
		t.Fatalf("group.json: %v", err)
	}
	// The manifest is a roster: the member's name is its directory, and
	// everything else about a member — settings, session, outcome — is read
	// from that member's own meta.json, never mirrored here.
	members := group["members"].([]any)
	if len(members) != 2 || members[0] != "codex" || members[1] != "claude-opus" {
		t.Fatalf("group.json members = %v", members)
	}
	if group["runnerLock"] != true {
		t.Fatalf("group.json runnerLock = %v, want true", group["runnerLock"])
	}
	// One stop, naming the process every member runs in, covers the set.
	stop := fmt.Sprintf("stop: kill -INT %d", int(group["runnerPid"].(float64)))
	if n := strings.Count(res.stdout, "stop: "); n != 1 || !strings.Contains(res.stdout, stop) {
		t.Fatalf("dispatch must print %q once, printed %d stop lines:\n%s", stop, n, res.stdout)
	}

	locks, _ := os.ReadDir(filepath.Join(e.home, ".local", "state", "envoy", "locks"))
	if len(locks) != 0 {
		t.Fatalf("every member's session lock must be released, found %d", len(locks))
	}
}

// The case the whole feature exists for: one member answers, the other does
// not. Exit 6 says both things are true at once, and collection prescribes per
// member — never a group-wide retry that would re-send an accepted prompt.
func TestFanPartialOutcomeCollectsPerMember(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO_CODEX", "success").
		set("ENVOY_FAKE_SCENARIO_CLAUDE", "init-only-hang")
	base := t.TempDir()
	outDir := filepath.Join(base, "20260726-120000-consult")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--with", "claude",
		"--baseline", "deadbeef", "--timeout-min", "0.02")...)
	if res.code != 6 {
		t.Fatalf("exit = %d, want 6 (partial)\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"status: partial — 1 of 2 turns returned a result",
		"member codex: ok · result ",
		"member claude: timeout — the wall-clock cap elapsed, which is not evidence the provider hung",
		"licenses nothing about another",
		"never the whole fan-out",
	)

	// Discovery finds the fan-out as one entry to act on, not two loose jobs.
	pending := runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending stdout", pending.stdout,
		"pending jobs: 1",
		"[group] "+outDir,
		"2 of 2 members still need attention",
		"codex: terminal status ok has not been collected",
		"claude: terminal status timeout has not been collected",
		"next: envoy collect")

	collected := runEnvoy(t, e, "collect", outDir)
	if collected.code != 0 {
		t.Fatalf("collect = %d\n%s", collected.code, collected.stderr)
	}
	mustContain(t, "collect stdout", collected.stdout,
		"fan-out: "+outDir,
		"status: partial — 1 of 2 turns returned a result",
		"members: codex ok · claude timeout",
		"=== member codex ===",
		"job: "+filepath.Join(outDir, "codex"),
		"fake provider result",
		"=== member claude ===",
		"status: timeout — the wall-clock cap elapsed",
		// the timed-out member's own recovery, not the group's
		"silence is not proof that nothing ran",
		"envoy run <new-job-name> --with @'"+filepath.Join(outDir, "claude")+"'",
		"next: the member results above are usable as they are",
	)
	// The ok member keeps its own single-turn closing line inside its section.
	mustContain(t, "collect stdout", collected.stdout, "result.md above is this turn's return value")
	// Every member reviewed the same range, so the diff is reported once for the
	// fan-out — repeating it per member would bury the findings in duplicates.
	if n := strings.Count(collected.stdout, "git since baseline deadbeef"); n != 1 {
		t.Fatalf("the reviewed range must print once per fan-out, got %d", n)
	}

	for _, name := range []string{"codex", "claude"} {
		if readMeta(t, filepath.Join(outDir, name))["collectedAt"] == nil {
			t.Fatalf("collecting the fan-out must stamp member %s", name)
		}
	}
	pending = runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending after collect", pending.stdout, "pending jobs: 0")
}

// One signal stops every member: the fan-out is one job to interrupt, and no
// member is left running behind a process the caller thinks it stopped.
func TestFanInterruptStopsEveryMember(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "delayed-success").
		set("ENVOY_FAKE_START_DELAY_MS", "0").
		set("ENVOY_FAKE_DELAY_MS", "60000")
	outDir := filepath.Join(t.TempDir(), "group")
	prompt := writePrompt(t, t.TempDir())

	cmd := exec.Command(binPath, runArgs(prompt, outDir, "--with", "codex", "--with", "claude", "--timeout-min", "5")...)
	cmd.Env = e.build()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Interrupt once both members have provably accepted their prompt.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if time.Now().After(deadline) {
			killDispatch(cmd, outDir)
			t.Fatalf("members never accepted; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
		accepted := 0
		for _, name := range []string{"codex", "claude"} {
			if data, err := os.ReadFile(filepath.Join(outDir, name, "meta.json")); err == nil &&
				strings.Contains(string(data), `"promptState": "accepted"`) {
				accepted++
			}
		}
		if accepted == 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGINT)
	code := 0
	if ee, ok := cmd.Wait().(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if code != 5 {
		t.Fatalf("exit = %d, want 5\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	mustContain(t, "stdout", stdout.String(),
		"received SIGINT: stopping all 2 turns",
		"status: no-result — none of the 2 turns returned a result")
	for _, name := range []string{"codex", "claude"} {
		meta := readMeta(t, filepath.Join(outDir, name))
		if meta["status"] != "interrupted" || meta["interruptionSignal"] != "SIGINT" {
			t.Fatalf("%s meta = status %v signal %v", name, meta["status"], meta["interruptionSignal"])
		}
	}
}

// The same model dispatched twice is a legitimate fan-out, so members are named
// for what distinguishes them and a repeat is numbered rather than merged.
func TestFanDuplicateMembersGetDistinctJobDirs(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "group")
	prompt := writePrompt(t, t.TempDir())

	// The third voice's model spells the name the numbering would give the
	// second: a generated name must never land on a directory another member
	// already owns, or two turns overwrite one answer.
	res := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "claude:opus", "--with", "claude:opus",
		"--with", "claude:opus-2", "--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	members := readGroup(t, outDir)["members"].([]any)
	if len(members) != 3 {
		t.Fatalf("group.json members = %v", members)
	}
	seen := map[string]bool{}
	for _, m := range members {
		name := m.(string)
		if seen[name] {
			t.Fatalf("two members share the address %q: %v", name, members)
		}
		seen[name] = true
		if readMeta(t, filepath.Join(outDir, name))["status"] != "ok" {
			t.Fatalf("%s did not run as its own turn", name)
		}
		mustContain(t, "stdout", res.stdout, "member "+name+": ok")
	}
	if !seen["claude-opus"] || !seen["claude-opus-2"] {
		t.Fatalf("the first pair keeps its numbering: %v", seen)
	}
}

// A finished fan-out continues as a set: every member resumed in its own
// session on one NEW prompt, dispatched and supervised as a new fan-out. The
// roster, each member's session, and the settings come from the original
// fan-out's records, so the caller re-decides nothing per member.
func TestFanResumeFromContinuesEveryMember(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "sess-codex-r1").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "sess-claude-r1")
	r1 := filepath.Join(t.TempDir(), "round1")
	prompt := writePrompt(t, t.TempDir())

	res := runEnvoy(t, e, runArgs(prompt, r1, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5")...)
	if res.code != 0 {
		t.Fatalf("round 1 exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}

	// Collecting the finished set offers the set-level follow-up, runnable as
	// printed, with the prompt file left as the placeholder a round 2 must fill.
	col := runEnvoy(t, e, "collect", r1)
	mustContain(t, "collect stdout", col.stdout,
		"resume: envoy run <new-job-name> --with @'"+r1+"' --timeout-min 5 --prompt-file <your-follow-up.md>")

	round2 := filepath.Join(t.TempDir(), "round2.md")
	if err := os.WriteFile(round2, []byte("round-2 prompt body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := filepath.Join(t.TempDir(), "round2-group")
	res = runEnvoy(t, e, "run", r2, "--with", "@"+r1, "--prompt-file", round2, "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("round 2 exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	// The members keep their identities, and each says which conversation
	// it continues.
	mustContain(t, "round 2 stdout", res.stdout,
		"member codex: model (provider default) · effort (provider default) · dir "+filepath.Join(r2, "codex")+" · continues "+filepath.Join(r1, "codex"),
		"member claude-opus: model opus · effort (provider default) · dir "+filepath.Join(r2, "claude-opus")+" · continues "+filepath.Join(r1, "claude-opus"),
		"status: ok — all 2 turns returned a result",
	)
	// Each member's turn is a real resume — the provider argv carries the
	// original session — and the roster's settings carry over.
	codexMeta := readMeta(t, filepath.Join(r2, "codex"))
	mustContain(t, "codex argv", fmt.Sprintf("%v", codexMeta["providerArgv"]),
		"resume", "sess-codex-r1")
	claudeMeta := readMeta(t, filepath.Join(r2, "claude-opus"))
	mustContain(t, "claude argv", fmt.Sprintf("%v", claudeMeta["providerArgv"]),
		"--resume", "sess-claude-r1")
	if claudeMeta["model"] != "opus" {
		t.Fatalf("claude-opus model = %v, want opus carried from the roster", claudeMeta["model"])
	}
	// Each resumed member also records which job its conversation continues.
	if codexMeta["resumedFrom"] != filepath.Join(r1, "codex") ||
		claudeMeta["resumedFrom"] != filepath.Join(r1, "claude-opus") {
		t.Fatalf("member lineage = %v / %v, want the original member dirs under %s",
			codexMeta["resumedFrom"], claudeMeta["resumedFrom"], r1)
	}
	// And every member was sent the NEW prompt, never the original again.
	for _, name := range []string{"codex", "claude-opus"} {
		if got := readFile(t, filepath.Join(r2, name, "prompt.md")); got != "round-2 prompt body\n" {
			t.Fatalf("%s prompt.md = %q", name, got)
		}
	}

	// The prompt may ride on the reference itself: @<fan-out>=<file> is the
	// same round with no --prompt-file, and the '=' attaches to the whole set.
	round3 := filepath.Join(t.TempDir(), "round3.md")
	if err := os.WriteFile(round3, []byte("round-3 prompt body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r3 := filepath.Join(t.TempDir(), "round3-group")
	res = runEnvoy(t, e, "run", r3, "--with", "@"+r2+"="+round3, "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("round 3 exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	for _, name := range []string{"codex", "claude-opus"} {
		if got := readFile(t, filepath.Join(r3, name, "prompt.md")); got != "round-3 prompt body\n" {
			t.Fatalf("round 3 %s prompt.md = %q", name, got)
		}
		if got := readMeta(t, filepath.Join(r3, name))["resumedFrom"]; got != filepath.Join(r2, name) {
			t.Fatalf("round 3 %s resumedFrom = %v", name, got)
		}
	}
}

// The resume set is whole or refused: a member that never published a session
// has no conversation to continue, and quietly dropping that voice would turn
// a two-voice round into a one-voice round without a record of why.
func TestFanResumeFromRefusesIncompleteSet(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO_CODEX", "exit-before-stdin").
		set("ENVOY_FAKE_SCENARIO_CLAUDE", "success")
	r1 := filepath.Join(t.TempDir(), "round1")
	prompt := writePrompt(t, t.TempDir())
	res := runEnvoy(t, e, runArgs(prompt, r1, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5")...)
	if res.code != 6 {
		t.Fatalf("mixed fan exit = %d, want 6\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}

	res = runEnvoy(t, e, runArgs(prompt, filepath.Join(t.TempDir(), "round2"), "--with", "@"+r1)...)
	if res.code != 3 {
		t.Fatalf("incomplete-set continuation exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr,
		"cannot be continued as a set",
		"member codex never published a session id")
}

// A fan-out's result-only keeps the aggregate line and the member split —
// attribution is the point of a fan-out — and drops the coordinate preamble.
func TestFanCollectResultOnly(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	outDir := filepath.Join(t.TempDir(), "group")
	prompt := writePrompt(t, t.TempDir())
	if r := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("fan exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	res := runEnvoy(t, e, "collect", "--result-only", outDir)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stdout", res.stdout,
		"status: ok — all 2 turns returned a result",
		"=== member codex ===",
		"=== member claude-opus ===",
		"fake provider result",
	)
	for _, banned := range []string{"fan-out:", "members:", "prompt:", "provider:", "next:"} {
		if strings.Contains(res.stdout, banned) {
			t.Fatalf("group result-only must not print %q:\n%s", banned, res.stdout)
		}
	}
}

// A member's session held by another live turn (a caller resumed it
// individually, say) refuses that member alone: the runner's own lock is
// the one gate, the sibling runs, and the round reports partial with the
// refused member marked never dispatched.
func TestFanResumeFromRefusesTheHeldMemberOnly(t *testing.T) {
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "sess-codex-held").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "sess-claude-free")
	r1 := filepath.Join(t.TempDir(), "round1")
	prompt := writePrompt(t, t.TempDir())
	if res := runEnvoy(t, e, runArgs(prompt, r1, "--with", "codex", "--with", "claude:opus",
		"--timeout-min", "5")...); res.code != 0 {
		t.Fatalf("round 1 exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	lockDir := filepath.Join(e.home, ".local", "state", "envoy", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"pid":%d,"runnerInstanceId":"another-turn","outDir":"%s","startedAt":"2026-07-28T00:00:00.000Z"}`,
		os.Getpid(), t.TempDir())
	if err := os.WriteFile(filepath.Join(lockDir, "sess-codex-held.lock"), []byte(payload+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r2 := filepath.Join(t.TempDir(), "round2")
	res := runEnvoy(t, e, runArgs(prompt, r2, "--with", "@"+r1, "--timeout-min", "5")...)
	if res.code != 6 {
		t.Fatalf("held-member resume exit = %d, want 6 (partial)\nstdout:\n%s\nstderr:\n%s",
			res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "[codex] lock error:", "sess-codex-held")
	mustContain(t, "stdout", res.stdout, "member codex: never dispatched", "member claude-opus: ok")
	if _, err := os.Stat(filepath.Join(r2, "codex", "meta.json")); err == nil {
		t.Fatal("the held member must not have run")
	}
	if readMeta(t, filepath.Join(r2, "claude-opus"))["status"] != "ok" {
		t.Fatal("the free member runs")
	}
}

// "Resumable" has one definition. A member whose turn ended in a session-lock
// conflict does not advertise a resume command of its own, so the set-level
// resume must not advertise or accept it either.
func TestConflictedMemberIsNotSetResumable(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "group")
	codexDir := filepath.Join(dir, "codex")
	opusDir := filepath.Join(dir, "claude-opus")
	for _, d := range []string{codexDir, opusDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cwd := t.TempDir()
	group := fmt.Sprintf(`{"schemaVersion":2,"startedAt":"2026-07-28T00:00:00.000Z","cwd":%q,"timeoutMin":5,"gitBaseline":null,"members":["codex","claude-opus"]}`, cwd)
	if err := os.WriteFile(filepath.Join(dir, "group.json"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
	okMeta := `{"schemaVersion":10,"status":"ok","provider":"codex","sessionId":"sess-ok","promptState":"accepted","timeoutMin":5,"collectedAt":null}`
	conflictMeta := `{"schemaVersion":10,"status":"failed","provider":"claude","model":"opus","sessionId":"sess-conflict","sessionLockConflict":{"sessionId":"sess-conflict","holder":{"pid":1,"startedAt":"2026-07-28T00:00:00.000Z","dir":"/tmp/other-job"},"holderLive":true},"promptState":"unknown","timeoutMin":5,"collectedAt":null}`
	os.WriteFile(filepath.Join(codexDir, "meta.json"), []byte(okMeta), 0o644)
	os.WriteFile(filepath.Join(opusDir, "meta.json"), []byte(conflictMeta), 0o644)
	os.WriteFile(filepath.Join(codexDir, "result.md"), []byte("codex answer"), 0o644)
	os.WriteFile(filepath.Join(opusDir, "result.md"), []byte("opus partial"), 0o644)

	col := runEnvoy(t, e, "collect", dir)
	if strings.Contains(col.stdout, "--with @'"+dir+"'") {
		t.Fatalf("a conflicted member must suppress the set-level resume line:\n%s", col.stdout)
	}

	prompt := writePrompt(t, t.TempDir())
	res := runEnvoy(t, e, runArgs(prompt, filepath.Join(t.TempDir(), "next"), "--with", "@"+dir)...)
	if res.code != 3 {
		t.Fatalf("conflicted-member resume exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "claude-opus", "session-lock conflict")
}

// A fan-out's closing line may not claim every result is usable while an ok
// member's payload never reached the caller: delivery, not status, is what
// the group's own next action must aggregate.
func TestFanCollectFlagsUndeliveredOkResult(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "group")
	codexDir := filepath.Join(dir, "codex")
	opusDir := filepath.Join(dir, "claude-opus")
	for _, d := range []string{codexDir, opusDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cwd := t.TempDir()
	group := fmt.Sprintf(`{"schemaVersion":2,"startedAt":"2026-07-28T00:00:00.000Z","cwd":%q,"timeoutMin":5,"gitBaseline":null,"members":["codex","claude-opus"]}`, cwd)
	if err := os.WriteFile(filepath.Join(dir, "group.json"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
	okMeta := `{"schemaVersion":10,"status":"ok","provider":"%s","sessionId":"sess-%s","promptState":"accepted","timeoutMin":5,"collectedAt":null}`
	os.WriteFile(filepath.Join(codexDir, "meta.json"), []byte(fmt.Sprintf(okMeta, "codex", "a")), 0o644)
	os.WriteFile(filepath.Join(opusDir, "meta.json"), []byte(fmt.Sprintf(okMeta, "claude", "b")), 0o644)
	// codex delivered; claude-opus reports ok but its payload is gone.
	os.WriteFile(filepath.Join(codexDir, "result.md"), []byte("codex answer"), 0o644)

	res := runEnvoy(t, e, "collect", dir)
	if res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if strings.Contains(res.stdout, "the member results above are this fan-out") {
		t.Fatalf("group closing claims usable results over an undelivered payload:\n%s", res.stdout)
	}
	mustContain(t, "stdout", res.stdout, "could not be read")
	if readMeta(t, opusDir)["collectedAt"] != nil {
		t.Fatal("the undelivered member must stay uncollected")
	}
	if readMeta(t, codexDir)["collectedAt"] == nil {
		t.Fatal("the delivered member must stamp as usual")
	}
}

// A fan-out is one job over N ordinary turns, and nothing about that needs
// the turns to share a prompt: a voice may carry its own file, and the job's
// --prompt-file is the default for the rest. Each member archives and sends
// exactly the file it was given.
func TestFanMembersReceiveTheirOwnPrompts(t *testing.T) {
	receipts := t.TempDir()
	e := newEnv(t).
		set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_PROMPT_FILE_CODEX", filepath.Join(receipts, "codex.txt")).
		set("ENVOY_FAKE_PROMPT_FILE_CLAUDE", filepath.Join(receipts, "claude.txt"))
	outDir := filepath.Join(t.TempDir(), "group")
	src := t.TempDir()
	landscape := filepath.Join(src, "landscape.md")
	critique := filepath.Join(src, "critique.md")
	os.WriteFile(landscape, []byte("survey the landscape\n"), 0o644)
	os.WriteFile(critique, []byte("critique the design\n"), 0o644)

	// A mixed roster: codex names its file, claude falls back to the default.
	res := runEnvoy(t, e, "run", outDir, "--with", "codex="+landscape, "--with", "claude:opus",
		"--prompt-file", critique, "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "fan-out: 2 turns · hard cap 5m each", "status: ok — all 2 turns returned a result")
	for name, want := range map[string]string{"codex": "survey the landscape\n", "claude-opus": "critique the design\n"} {
		if got := readFile(t, filepath.Join(outDir, name, "prompt.md")); got != want {
			t.Fatalf("%s prompt.md = %q, want %q", name, got, want)
		}
	}
	// The archive proves the copy; the receipt proves the bytes reached stdin.
	if got := readFile(t, filepath.Join(receipts, "codex.txt")); got != "survey the landscape\n" {
		t.Fatalf("codex received %q", got)
	}
	if got := readFile(t, filepath.Join(receipts, "claude.txt")); got != "critique the design\n" {
		t.Fatalf("claude received %q", got)
	}
	if _, err := os.Stat(filepath.Join(outDir, "prompt.md")); !os.IsNotExist(err) {
		t.Fatal("a fan-out with differing prompts can hold no group prompt.md")
	}

	// Every voice carrying its own file needs no default at all.
	both := filepath.Join(t.TempDir(), "both")
	res = runEnvoy(t, e, "run", both, "--with", "codex="+landscape, "--with", "claude:opus="+critique, "--timeout-min", "5")
	if res.code != 0 {
		t.Fatalf("no-default exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if got := readFile(t, filepath.Join(both, "claude-opus", "prompt.md")); got != "critique the design\n" {
		t.Fatalf("claude-opus prompt.md = %q", got)
	}
}

// The roster is the only evidence a member was meant to run. A member that
// never wrote its record is invisible to its own files, so the group carries
// it in pending rather than letting the fan-out fall out of the index once its
// siblings are collected.
func TestPendingCarriesARosterMemberWithoutARecord(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	base := t.TempDir()
	outDir := filepath.Join(base, "group")
	prompt := writePrompt(t, t.TempDir())
	if r := runEnvoy(t, e, runArgs(prompt, outDir, "--with", "codex", "--with", "claude:opus", "--timeout-min", "5")...); r.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", r.code, r.stderr)
	}
	if r := runEnvoy(t, e, "collect", outDir); r.code != 0 {
		t.Fatalf("collect = %d\n%s", r.code, r.stderr)
	}
	pending := runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending after collect", pending.stdout, "pending jobs: 0")

	if err := os.Remove(filepath.Join(outDir, "claude-opus", "meta.json")); err != nil {
		t.Fatal(err)
	}
	pending = runEnvoy(t, e, "pending", "--base", base)
	mustContain(t, "pending stdout", pending.stdout,
		"pending jobs: 1",
		"[group] "+outDir,
		"1 of 2 members still need attention",
		"claude-opus: has no meta.json, so it never recorded a start")

	// Collect says only what the directory shows — no record, outcome unknown,
	// read the logs — and never that nothing ran, which it did not observe.
	col := runEnvoy(t, e, "collect", outDir)
	if col.code != 0 {
		t.Fatalf("collect = %d\n%s", col.code, col.stderr)
	}
	mustContain(t, "collect stdout", col.stdout,
		"members: codex ok · claude-opus no status",
		"=== member claude-opus ===",
		"status: no record — this member never wrote meta.json",
		"next: read "+filepath.Join(outDir, "claude-opus", "progress.log"))
	mustNotContain(t, "collect stdout", col.stdout, "never dispatched", "nothing ran for it")
	mustNotContain(t, "collect stderr", col.stderr, "collect error")
}

// A member that wrote a record started, whether or not this engine can read
// the record — damaged, or another version's. Its section says so and sends
// the reader to its files; it is never glossed as a member nothing ran for.
func TestAnUnreadableMemberIsNeverCalledUndispatched(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "group")
	group := fmt.Sprintf(`{"schemaVersion":2,"startedAt":"2026-09-01T00:00:00.000Z","cwd":%q,"timeoutMin":5,"gitBaseline":null,"members":["codex"]}`, t.TempDir())
	member := filepath.Join(dir, "codex")
	if err := os.MkdirAll(member, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "group.json"), []byte(group), 0o644)
	os.WriteFile(filepath.Join(member, "meta.json"), []byte(`{"schemaVersion":9,"status":"ok","provider":"codex","collectedAt":null}`), 0o644)

	col := runEnvoy(t, e, "collect", dir)
	mustContain(t, "collect stdout", col.stdout,
		"status: unreadable — this member wrote meta.json, so it started",
		"next: read "+filepath.Join(member, "result.md"))
	mustNotContain(t, "collect stdout", col.stdout, "never dispatched", "nothing ran for it")
	mustContain(t, "collect stderr", col.stderr, "schema 9")
}

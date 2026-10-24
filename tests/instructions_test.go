// Standing instructions: the system-level text a conversation is started
// with. Every job of the conversation archives it and every launch passes it
// again, because both providers keep the first text only until a compaction
// and then take whatever that launch passed. The conversation owns the text:
// a continuation inherits it from the source job's records, never from what
// the caller passes, and a conversation whose archive no longer supplies it
// does not continue at all.
package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeInstructions(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rules.md")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// argAfter is the argument following flag in argv, "" when flag is absent.
func argAfter(argv []string, flag string) string {
	if i := slices.Index(argv, flag); i >= 0 && i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

// A claude conversation started with --instructions-file hands the provider
// its job's own archive on the first turn and on every turn that continues
// it, with or without the flag — and the prompt archive stays the turn's job
// alone.
func TestStandingInstructionsRideEveryTurnOfAClaudeConversation(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "invocation.json")
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_INVOCATION_FILE", capture)
	text := "Work unattended: nobody answers mid-turn.\nProduction is read-only.\n"
	rules := writeInstructions(t, text)
	prompt := writePrompt(t, t.TempDir())

	r1 := filepath.Join(t.TempDir(), "design")
	res := runEnvoy(t, e, runArgs(prompt, r1, "--with", "claude", "--instructions-file", rules)...)
	if res.code != 0 {
		t.Fatalf("first turn exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}

	// Each turn: its own archive, the record's digest of it, the archive as
	// what claude was pointed at, and the prompt untouched.
	check := func(name, dir string, stdout string) {
		t.Helper()
		archive := filepath.Join(dir, "instructions.md")
		mustContain(t, name+" stdout", stdout, "instructions: "+archive)
		if got := readFile(t, archive); got != text {
			t.Fatalf("%s instructions.md = %q, want %q", name, got, text)
		}
		meta := readMeta(t, dir)
		if meta["instructionsSha256"] != digest(text) {
			t.Fatalf("%s instructionsSha256 = %v, want %s", name, meta["instructionsSha256"], digest(text))
		}
		if got := argAfter(stringsFromMeta(t, meta, "providerArgv"), "--append-system-prompt-file"); got != archive {
			t.Fatalf("%s recorded argv passes %q, want the job's own archive %s", name, got, archive)
		}
		inv := readInvocation(t, capture)
		if inv.AppendedSystemPrompt == nil || *inv.AppendedSystemPrompt != text {
			t.Fatalf("%s: the provider was handed %v, want the instructions", name, inv.AppendedSystemPrompt)
		}
		if got := readFile(t, filepath.Join(dir, "prompt.md")); strings.Contains(got, "Production is read-only") {
			t.Fatalf("%s prompt.md carries the instructions: %q", name, got)
		}
	}
	check("first turn", r1, res.stdout)

	followUp := filepath.Join(t.TempDir(), "build.md")
	if err := os.WriteFile(followUp, []byte("build it\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := filepath.Join(t.TempDir(), "build")
	res = runEnvoy(t, e, "run", r2, "--with", "@"+r1, "--prompt-file", followUp)
	if res.code != 0 {
		t.Fatalf("continuation exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	check("continuation", r2, res.stdout)
	if got := readFile(t, filepath.Join(r2, "prompt.md")); got != "build it\n" {
		t.Fatalf("continuation prompt.md = %q, want the NEW prompt only", got)
	}

	// A caller that passes the same file on every turn is passing no change.
	r3 := filepath.Join(t.TempDir(), "review")
	res = runEnvoy(t, e, "run", r3, "--with", "@"+r2, "--prompt-file", followUp, "--instructions-file", rules)
	if res.code != 0 {
		t.Fatalf("identical re-pass exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	check("identical re-pass", r3, res.stdout)

	// The archive is a setting like the model: held back from a delivered
	// turn's block, printed on the full read.
	mustContain(t, "status-only", runEnvoy(t, e, "collect", "--status-only", r3).stdout,
		"instructions: "+filepath.Join(r3, "instructions.md"))
	mustNotContain(t, "delivered block", runEnvoy(t, e, "collect", r3).stdout, "instructions:")
}

// A conversation keeps the instructions it started with: a different file,
// or any file for a conversation that started without one, is refused before
// anything is reserved — the session would go on holding its first text. A
// conversation that never had any continues as it is.
func TestContinuationRefusesChangedOrAddedInstructions(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())
	rules := writeInstructions(t, "rule A\n")
	other := writeInstructions(t, "rule B\n")

	bare := filepath.Join(t.TempDir(), "bare")
	if res := runEnvoy(t, e, runArgs(prompt, bare, "--with", "claude")...); res.code != 0 {
		t.Fatalf("bare exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	ruled := filepath.Join(t.TempDir(), "ruled")
	if res := runEnvoy(t, e, runArgs(prompt, ruled, "--with", "claude", "--instructions-file", rules)...); res.code != 0 {
		t.Fatalf("ruled exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	cases := []struct {
		with, file string
		want       []string
	}{
		{bare, rules, []string{"--with @" + bare + ": that conversation started without standing instructions",
			"Continue it without --instructions-file"}},
		{ruled, other, []string{"--with @" + ruled + ": --instructions-file differs from the standing instructions",
			filepath.Join(ruled, "instructions.md"), "Leave --instructions-file off"}},
	}
	for _, c := range cases {
		dir := filepath.Join(t.TempDir(), "next")
		res := runEnvoy(t, e, runArgs(prompt, dir, "--with", "@"+c.with, "--instructions-file", c.file)...)
		if res.code != 3 {
			t.Fatalf("@%s with %s: exit = %d, want 3\nstderr:\n%s", c.with, c.file, res.code, res.stderr)
		}
		mustContain(t, "refusal", res.stderr, c.want...)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("@%s: a refused run must leave no job dir", c.with)
		}
	}

	// Absence is not loss: the bare conversation continues bare.
	next := filepath.Join(t.TempDir(), "next")
	if res := runEnvoy(t, e, runArgs(prompt, next, "--with", "@"+bare)...); res.code != 0 {
		t.Fatalf("bare continuation exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	meta := readMeta(t, next)
	if _, ok := meta["instructionsSha256"]; ok || slices.Contains(stringsFromMeta(t, meta, "providerArgv"), "--append-system-prompt-file") {
		t.Fatalf("a conversation without instructions gains none: %v", meta["providerArgv"])
	}
	if _, err := os.Stat(filepath.Join(next, "instructions.md")); !os.IsNotExist(err) {
		t.Fatal("a conversation without instructions archives none")
	}
}

// An instructions file is checked like a prompt, before anything is reserved,
// and refused when it carries nothing a conversation could keep.
func TestInstructionsFileIsValidatedBeforeReserving(t *testing.T) {
	e := newEnv(t)
	prompt := writePrompt(t, t.TempDir())
	invalid := filepath.Join(t.TempDir(), "latin1.md")
	if err := os.WriteFile(invalid, []byte{'c', 'a', 'f', 0xe9, '\n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ file, want string }{
		{filepath.Join(t.TempDir(), "absent.md"), "instructions file not found"},
		{writeInstructions(t, " \n\t\n"), "instructions file is empty"},
		{invalid, "instructions file is not UTF-8 text"},
	}
	for _, c := range cases {
		dir := filepath.Join(t.TempDir(), "job")
		res := runEnvoy(t, e, runArgs(prompt, dir, "--with", "claude", "--instructions-file", c.file)...)
		if res.code != 3 {
			t.Fatalf("%s: exit = %d, want 3\nstderr:\n%s", c.file, res.code, res.stderr)
		}
		mustContain(t, "stderr", res.stderr, c.want)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s: a refused run must leave no job dir", c.file)
		}
	}
}

// A record that names instructions continues only with them. An archive that
// was lost or edited cannot supply them, so the conversation is refused at
// dispatch, and collect — whose resume line follows the same rule — offers
// no continuation and says why, whatever the tier.
func TestLostOrEditedArchiveBlocksTheConversation(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())
	rules := writeInstructions(t, "rule A\n")

	for _, c := range []struct {
		name    string
		damage  func(path string) error
		refusal string
		line    string
	}{
		{"lost", os.Remove, "is missing or will not read", "missing or unreadable, so this conversation cannot continue"},
		{"edited", func(p string) error { return os.WriteFile(p, []byte("rule A, softened\n"), 0o644) },
			"no longer holds the text it was sent", "edited since it was sent, so this conversation cannot continue"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src")
			if res := runEnvoy(t, e, runArgs(prompt, src, "--with", "claude", "--instructions-file", rules)...); res.code != 0 {
				t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
			}
			archive := filepath.Join(src, "instructions.md")
			if err := c.damage(archive); err != nil {
				t.Fatal(err)
			}

			dir := filepath.Join(t.TempDir(), "next")
			res := runEnvoy(t, e, runArgs(prompt, dir, "--with", "@"+src)...)
			if res.code != 3 {
				t.Fatalf("exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
			}
			mustContain(t, "refusal", res.stderr, "--with @"+src+": ", archive, c.refusal, "start a new conversation")
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("a refused run must leave no job dir")
			}

			col := runEnvoy(t, e, "collect", src).stdout
			mustContain(t, "collect", col, "instructions: "+archive+" — "+c.line, "session: ")
			mustNotContain(t, "collect", col, "resume:")
		})
	}
}

// A fan-out's cold members each start a conversation with the roster's
// instructions, archived in their own directory and spelled in their own
// provider's terms; a round continues every member with its own.
func TestFanOutMembersCarryTheirOwnInstructions(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "codex-session").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "claude-session")
	prompt := writePrompt(t, t.TempDir())
	text := "rule A\n"
	rules := writeInstructions(t, text)

	fan := filepath.Join(t.TempDir(), "consult")
	res := runEnvoy(t, e, runArgs(prompt, fan, "--with", "codex", "--with", "claude", "--instructions-file", rules)...)
	if res.code != 0 {
		t.Fatalf("fan exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "dispatch block", res.stdout, "member codex: ", "member claude: ", " · standing instructions")
	var group map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(fan, "group.json"))), &group); err != nil {
		t.Fatal(err)
	}
	for key := range group {
		if strings.Contains(strings.ToLower(key), "instructions") {
			t.Fatalf("group.json is a roster; a member's instructions live in the member's records, not %q", key)
		}
	}
	members := func(dir string) {
		t.Helper()
		codex, claude := filepath.Join(dir, "codex"), filepath.Join(dir, "claude")
		for _, m := range []string{codex, claude} {
			if got := readFile(t, filepath.Join(m, "instructions.md")); got != text {
				t.Fatalf("%s instructions.md = %q", m, got)
			}
		}
		if !slices.Contains(stringsFromMeta(t, readMeta(t, codex), "providerArgv"), `developer_instructions="rule A\n"`) {
			t.Fatalf("codex member argv = %v", readMeta(t, codex)["providerArgv"])
		}
		if got := argAfter(stringsFromMeta(t, readMeta(t, claude), "providerArgv"), "--append-system-prompt-file"); got != filepath.Join(claude, "instructions.md") {
			t.Fatalf("claude member passes %q, want its own archive", got)
		}
	}
	members(fan)

	round := filepath.Join(t.TempDir(), "consult-r2")
	if res := runEnvoy(t, e, runArgs(prompt, round, "--with", "@"+fan)...); res.code != 0 {
		t.Fatalf("round exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	members(round)
}

// A roster seating a new conversation beside a continued one gives the
// roster's instructions to the new one, and lets the continued one keep its
// own — and only its own, so a file that differs from them is refused.
func TestMixedRosterKeepsEachConversationsInstructions(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "codex-session").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "claude-session")
	prompt := writePrompt(t, t.TempDir())
	rules := writeInstructions(t, "rule A\n")

	src := filepath.Join(t.TempDir(), "design")
	if res := runEnvoy(t, e, runArgs(prompt, src, "--with", "claude", "--instructions-file", rules)...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	plain := filepath.Join(t.TempDir(), "plain")
	if res := runEnvoy(t, e, runArgs(prompt, plain, "--with", "@"+src, "--with", "codex")...); res.code != 0 {
		t.Fatalf("mixed exit = %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	if got := readFile(t, filepath.Join(plain, "claude", "instructions.md")); got != "rule A\n" {
		t.Fatalf("the continued member keeps its own: %q", got)
	}
	if _, err := os.Stat(filepath.Join(plain, "codex", "instructions.md")); !os.IsNotExist(err) {
		t.Fatal("a new conversation without the flag starts without instructions")
	}

	same := filepath.Join(t.TempDir(), "same")
	if res := runEnvoy(t, e, runArgs(prompt, same, "--with", "@"+src, "--with", "codex", "--instructions-file", rules)...); res.code != 0 {
		t.Fatalf("matching flag exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	for _, m := range []string{"claude", "codex"} {
		if got := readFile(t, filepath.Join(same, m, "instructions.md")); got != "rule A\n" {
			t.Fatalf("%s instructions.md = %q", m, got)
		}
	}
}

// The identical retry repeats the dispatch it replaces: a turn that started
// its conversation names its archived instructions again, while a retried
// continuation takes them from its source as any continuation does.
func TestRetryRepeatsTheArchivedInstructions(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_FAKE_SESSION_ID", "thread-1")
	prompt := writePrompt(t, t.TempDir())
	rules := writeInstructions(t, "rule A\n")

	src := filepath.Join(t.TempDir(), "src")
	if res := runEnvoy(t, e, runArgs(prompt, src, "--with", "codex", "--instructions-file", rules)...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	// A launcher that is not there never spawns, so the prompt provably
	// never reached the provider and the identical retry is offered.
	e.set("ENVOY_CODEX_CMD", "no-such-launcher-on-path")
	fresh := filepath.Join(t.TempDir(), "fresh")
	if res := runEnvoy(t, e, runArgs(prompt, fresh, "--with", "codex", "--instructions-file", rules)...); res.code != 2 {
		t.Fatalf("fresh exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	mustContain(t, "fresh retry", runEnvoy(t, e, "collect", fresh).stdout,
		fmt.Sprintf("--prompt-file '%s' --instructions-file '%s'",
			filepath.Join(fresh, "prompt.md"), filepath.Join(fresh, "instructions.md")))

	continued := filepath.Join(t.TempDir(), "continued")
	if res := runEnvoy(t, e, runArgs(prompt, continued, "--with", "@"+src)...); res.code != 2 {
		t.Fatalf("continued exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	col := runEnvoy(t, e, "collect", continued).stdout
	mustContain(t, "continued retry", col, "--with @'"+src+"'")
	mustNotContain(t, "continued retry", col, "--instructions-file")
}

// A record whose conversation carries standing instructions is written as a
// schema of its own. An engine that reads only the schema before it would
// decode such a record without the digest — and stamping a collect would then
// write it back without one, turning "this conversation has instructions" into
// "it never had any" for the next continuation; a version it does not read is
// refused by name instead (job_test.go). A record without instructions keeps
// the schema every engine since it reads alike.
func TestARecordWithInstructionsIsWrittenAsItsOwnSchema(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())

	ruled := filepath.Join(t.TempDir(), "ruled")
	if res := runEnvoy(t, e, runArgs(prompt, ruled, "--with", "claude", "--instructions-file", writeInstructions(t, "rule A\n"))...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	bare := filepath.Join(t.TempDir(), "bare")
	if res := runEnvoy(t, e, runArgs(prompt, bare, "--with", "claude")...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if got := readMeta(t, ruled)["schemaVersion"]; got != float64(11) {
		t.Fatalf("instructed record schemaVersion = %v, want 11", got)
	}
	if got := readMeta(t, bare)["schemaVersion"]; got != float64(10) {
		t.Fatalf("record without instructions schemaVersion = %v, want 10", got)
	}
}

// An archive edited after its turn never started is never handed to the
// identical retry as if it were what was sent: the block says the archive no
// longer holds the text, and the retry takes the caller's own copy in its
// place, with the digest that copy must match.
func TestRetryOfAnEditedArchiveTakesTheCallersOwnCopy(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").set("ENVOY_CODEX_CMD", "no-such-launcher-on-path")
	prompt := writePrompt(t, t.TempDir())
	fresh := filepath.Join(t.TempDir(), "fresh")
	if res := runEnvoy(t, e, runArgs(prompt, fresh, "--with", "codex", "--instructions-file", writeInstructions(t, "rule A\n"))...); res.code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	archive := filepath.Join(fresh, "instructions.md")
	if err := os.WriteFile(archive, []byte("rule B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	col := runEnvoy(t, e, "collect", fresh).stdout
	mustContain(t, "collect", col, "no longer holds the text it was sent", "sha256 is "+digest("rule A\n"),
		"--prompt-file '"+filepath.Join(fresh, "prompt.md")+"' --instructions-file <your-instructions.md>")
	mustNotContain(t, "collect", col, "--instructions-file '"+archive+"'")
}

// A refusal over a damaged archive hands over the repair as a command: the
// copy from an earlier turn of the conversation whose archive still holds the
// text, or — when no turn does — the new conversation to start in its place.
func TestADamagedArchiveRefusalNamesTheRepair(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success")
	prompt := writePrompt(t, t.TempDir())
	first := filepath.Join(t.TempDir(), "first")
	if res := runEnvoy(t, e, runArgs(prompt, first, "--with", "claude:opus", "--instructions-file", writeInstructions(t, "rule A\n"))...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	second := filepath.Join(t.TempDir(), "second")
	if res := runEnvoy(t, e, runArgs(prompt, second, "--with", "@"+first)...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}

	if err := os.Remove(filepath.Join(second, "instructions.md")); err != nil {
		t.Fatal(err)
	}
	res := runEnvoy(t, e, runArgs(prompt, filepath.Join(t.TempDir(), "x"), "--with", "@"+second)...)
	mustContain(t, "refusal with an intact earlier turn", res.stderr,
		"cp '"+filepath.Join(first, "instructions.md")+"' '"+filepath.Join(second, "instructions.md")+"'")

	if err := os.Remove(filepath.Join(first, "instructions.md")); err != nil {
		t.Fatal(err)
	}
	res = runEnvoy(t, e, runArgs(prompt, filepath.Join(t.TempDir(), "y"), "--with", "@"+second)...)
	mustContain(t, "refusal with no intact turn", res.stderr, "sha256 is "+digest("rule A\n"), "--with claude:opus")
	mustNotContain(t, "refusal with no intact turn", res.stderr, "cp '")
}

// One member's damaged archive blocks the round and nothing else: the set's
// resume line goes, the intact member keeps its own, and the round's refusal
// names the member, its archive and the repair — not a condition the members
// already meet.
func TestADamagedMemberArchiveBlocksTheRoundOnly(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "codex-session").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "claude-session")
	prompt := writePrompt(t, t.TempDir())
	fan := filepath.Join(t.TempDir(), "consult")
	if res := runEnvoy(t, e, runArgs(prompt, fan, "--with", "codex", "--with", "claude", "--instructions-file", writeInstructions(t, "rule A\n"))...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	codexArchive := filepath.Join(fan, "codex", "instructions.md")
	if err := os.Remove(codexArchive); err != nil {
		t.Fatal(err)
	}

	col := runEnvoy(t, e, "collect", "--status-only", fan).stdout
	mustNotContain(t, "group block", col, "--with @'"+fan+"'")
	mustContain(t, "group block", col, "resume: envoy run <new-job-name> --with @'"+filepath.Join(fan, "claude")+"'",
		"instructions: "+codexArchive+" — missing")

	res := runEnvoy(t, e, runArgs(prompt, filepath.Join(t.TempDir(), "r2"), "--with", "@"+fan)...)
	if res.code != 3 {
		t.Fatalf("round exit = %d, want 3\nstderr:\n%s", res.code, res.stderr)
	}
	// The members started their conversations together, so the sibling's
	// archive holds the same text by the digest: the repair is a copy.
	mustContain(t, "round refusal", res.stderr, "member codex", codexArchive,
		"cp '"+filepath.Join(fan, "claude", "instructions.md")+"' '"+codexArchive+"'")
	mustNotContain(t, "round refusal", res.stderr, "finished with a session")
}

// A round continues each member with its own conversation's instructions,
// even when the members' conversations started with different ones.
func TestARoundKeepsEachMembersOwnInstructions(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "codex-session").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "claude-session")
	prompt := writePrompt(t, t.TempDir())
	a := filepath.Join(t.TempDir(), "a")
	if res := runEnvoy(t, e, runArgs(prompt, a, "--with", "claude", "--instructions-file", writeInstructions(t, "rule A\n"))...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	b := filepath.Join(t.TempDir(), "b")
	if res := runEnvoy(t, e, runArgs(prompt, b, "--with", "codex", "--instructions-file", writeInstructions(t, "rule B\n"))...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	pair := filepath.Join(t.TempDir(), "pair")
	if res := runEnvoy(t, e, runArgs(prompt, pair, "--with", "@"+a, "--with", "@"+b)...); res.code != 0 {
		t.Fatalf("pair exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	round := filepath.Join(t.TempDir(), "round")
	if res := runEnvoy(t, e, runArgs(prompt, round, "--with", "@"+pair)...); res.code != 0 {
		t.Fatalf("round exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	for _, dir := range []string{pair, round} {
		if got := readFile(t, filepath.Join(dir, "claude", "instructions.md")); got != "rule A\n" {
			t.Fatalf("%s claude member = %q, want its own rule A", dir, got)
		}
		if got := readFile(t, filepath.Join(dir, "codex", "instructions.md")); got != "rule B\n" {
			t.Fatalf("%s codex member = %q, want its own rule B", dir, got)
		}
	}
}

// A member that left no record has no archive of its own, so its re-send is
// told to carry the standing instructions its new siblings were sent.
func TestAMemberWithoutARecordIsResentWithItsSiblingsInstructions(t *testing.T) {
	e := newEnv(t).set("ENVOY_FAKE_SCENARIO", "success").
		set("ENVOY_FAKE_SESSION_ID_CODEX", "codex-session").
		set("ENVOY_FAKE_SESSION_ID_CLAUDE", "claude-session")
	prompt := writePrompt(t, t.TempDir())
	fan := filepath.Join(t.TempDir(), "consult")
	if res := runEnvoy(t, e, runArgs(prompt, fan, "--with", "codex", "--with", "claude", "--instructions-file", writeInstructions(t, "rule A\n"))...); res.code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", res.code, res.stderr)
	}
	if err := os.Remove(filepath.Join(fan, "claude", "meta.json")); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "group block", runEnvoy(t, e, "collect", "--status-only", fan).stdout,
		"--instructions-file '"+filepath.Join(fan, "codex", "instructions.md")+"'")
}

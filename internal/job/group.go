package job

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// ExitPartial reports a fan-out where some members returned a result and
// others did not. It exists because neither 0 nor any failure code licenses
// the right move there: the results that landed are usable, and only the
// members that failed need a decision. Only a fan-out can exit this way — a
// single turn's codes are unchanged.
const ExitPartial = 6

// groupExitPrecedence orders the codes for a fan-out where no member returned
// a result, worst first. A dispatch-side cause outranks a provider-side one:
// rejected flags or a locked session, then an envoy/environment failure, then
// a stopped turn, the cap, and last the provider's own verdict. The earlier
// ones are fixed by changing the command, the later ones by waiting or
// changing the prompt.
var groupExitPrecedence = []int{ExitUsage, ExitInfra, ExitInterrupted, ExitTimeout, ExitFailed}

// ExitCodeForGroup aggregates the members' exit codes into the fan-out's own.
func ExitCodeForGroup(codes []int) int {
	if len(codes) == 0 {
		return ExitInfra
	}
	ok := 0
	for _, c := range codes {
		if c == ExitOK {
			ok++
		}
	}
	switch {
	case ok == len(codes):
		return ExitOK
	case ok > 0:
		return ExitPartial
	}
	for _, want := range groupExitPrecedence {
		if slices.Contains(codes, want) {
			return want
		}
	}
	return ExitInfra
}

// GroupSchemaVersion is the only group.json schema this engine reads or
// writes.
const GroupSchemaVersion = 2

// Group is the manifest of one fan-out: the members it dispatched, in roster
// order, and the settings they shared. A member's name is also its directory
// under the fan-out, so the roster is the only coordinate the manifest holds.
//
// It deliberately records no member status, session, settings, or result.
// Every member is an ordinary turn whose meta.json is the single source of
// that truth, so the manifest cannot drift out of step with the members it
// names. A reader wanting anything about a member reads the member.
type Group struct {
	SchemaVersion int      `json:"schemaVersion"`
	StartedAt     string   `json:"startedAt"`
	Cwd           string   `json:"cwd"`
	TimeoutMin    float64  `json:"timeoutMin"`
	GitBaseline   *string  `json:"gitBaseline"`
	Members       []string `json:"members"`
}

// Marshal renders the canonical on-disk form.
func (g *Group) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// WriteFile atomically replaces the fan-out's group.json.
func (g *Group) WriteFile(path string) error {
	data, err := g.Marshal()
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data)
}

// ReadGroupFile parses a group.json this engine wrote.
func ReadGroupFile(path string) (*Group, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g Group
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	if g.SchemaVersion != GroupSchemaVersion {
		return nil, fmt.Errorf("group.json is schema %d and this engine reads schema %d only", g.SchemaVersion, GroupSchemaVersion)
	}
	return &g, nil
}

// GroupWorkspace is one fan-out directory and the fixed names inside it. Each
// member gets an ordinary job directory one level down, named after the member,
// so every single-turn path in this package keeps working unchanged.
type GroupWorkspace struct {
	Dir string
}

func (g GroupWorkspace) GroupPath() string  { return filepath.Join(g.Dir, "group.json") }
func (g GroupWorkspace) PromptPath() string { return filepath.Join(g.Dir, "prompt.md") }

// Member is the job workspace of one named member.
func (g GroupWorkspace) Member(name string) Workspace {
	return Workspace{Dir: filepath.Join(g.Dir, name)}
}

// Prepare copies the shared prompt into the fan-out directory, so the group
// records exactly what every member was sent even if a member dir is lost.
func (g GroupWorkspace) Prepare(promptFile string) error {
	return copyFile(promptFile, g.PromptPath())
}

// IsGroupDir reports whether a job directory is a fan-out rather than a single
// turn. The presence of group.json is the discriminator, and it is written
// before any member starts.
func IsGroupDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "group.json"))
	return err == nil
}

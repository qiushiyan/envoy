package job

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// Standing instructions are the system-level text a conversation was started
// with. They belong to the conversation: every turn of it is sent the same
// text and archives it as instructions.md, and its record carries the digest,
// so a continuation re-passes exactly what the conversation started with —
// read from the source job's archive, never from what a caller remembers.

// InstructionsProblem is what keeps a turn's archive from carrying its
// conversation's instructions on: "" when the archive holds what the record
// names, or when the record names none.
type InstructionsProblem string

const (
	// InstructionsMissing: the record names instructions and the archive is
	// gone or will not read.
	InstructionsMissing InstructionsProblem = "missing"
	// InstructionsChanged: the archive no longer holds the bytes the record's
	// digest names.
	InstructionsChanged InstructionsProblem = "changed"
)

// Digest is the hex SHA-256 a record keeps of the instructions it was sent.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ReadInstructions returns the standing instructions the turn in dir was
// sent, verified against its record. A record without a digest had none, and
// reads as "" with no problem: absence is not loss. A record with one is only
// as good as its archive, so a missing or edited archive is a problem, never
// an empty text.
func ReadInstructions(dir string, m *Meta) (string, InstructionsProblem) {
	if m == nil || m.InstructionsSha256 == nil {
		return "", ""
	}
	data, err := os.ReadFile(Workspace{Dir: dir}.InstructionsPath())
	switch {
	case err != nil:
		return "", InstructionsMissing
	case Digest(string(data)) != *m.InstructionsSha256:
		return "", InstructionsChanged
	}
	return string(data), ""
}

// IntactInstructionsSource is another turn's archive that still holds the
// text the record m read from dir names — what a repair of dir's archive can
// copy from — or "" when none does. Every turn of a conversation archives the
// same text, so its lineage (resumedFrom, back to the first turn) is searched
// first; then, for a fan-out member, its siblings, whose new conversations were
// started with the same instructions. A candidate counts only when its record
// carries the same digest and its archive verifies against it.
func IntactInstructionsSource(dir string, m *Meta) string {
	if m == nil || m.InstructionsSha256 == nil {
		return ""
	}
	intact := func(candidate string, cm *Meta) bool {
		if cm == nil || cm.InstructionsSha256 == nil || *cm.InstructionsSha256 != *m.InstructionsSha256 {
			return false
		}
		_, problem := ReadInstructions(candidate, cm)
		return problem == ""
	}
	seen := map[string]bool{dir: true}
	for from := m.ResumedFrom; from != nil && !seen[*from]; {
		seen[*from] = true
		src, err := ReadMeta(*from)
		if err != nil {
			break
		}
		if intact(*from, src) {
			return Workspace{Dir: *from}.InstructionsPath()
		}
		from = src.ResumedFrom
	}
	if group := filepath.Dir(dir); IsGroupDir(group) {
		if fan, err := ReadFan(group); err == nil {
			for _, sibling := range fan.Members {
				if !seen[sibling.Dir] && intact(sibling.Dir, sibling.Meta) {
					return Workspace{Dir: sibling.Dir}.InstructionsPath()
				}
			}
		}
	}
	return ""
}

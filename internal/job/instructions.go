package job

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
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

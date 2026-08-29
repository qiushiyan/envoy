package job

import (
	"os"
	"path/filepath"
)

// WriteCoordinateFile lands the dispatch coordinate block at path as one
// atomic rename, so a caller blocking on the file's existence never reads a
// half-written block. The file is the caller's correlated handoff channel:
// a background dispatch's stdout is invisible to the harness until the
// process exits, and "the newest job" cannot prove which dispatch it means.
func WriteCoordinateFile(path string, block []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, block, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

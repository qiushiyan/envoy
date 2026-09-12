package job

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A job is named by its caller, before it runs, so the caller never has to
// read a coordinate back from a dispatch whose stdout the harness hides.
// The name is an address: it must round-trip exactly, so it is neither
// lowercased nor slugged, and it is reserved by creating the directory —
// creation is the collision check, with no stat-then-create window.

// namePattern is what a bare job name may look like: one path segment,
// starting with a letter or digit. Anything with a separator is a path.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// IsPath reports whether a job argument names a directory rather than a
// project-scoped name: it holds a path separator or starts with a dot.
func IsPath(arg string) bool {
	return strings.ContainsRune(arg, os.PathSeparator) || strings.HasPrefix(arg, ".")
}

// ResolveRef turns a job argument into the directory it names: a bare name
// lands in the invoking project's store, a path is taken as given. The
// project is the one the caller runs in — never the tree a turn is told to
// work in — so a name resolves the same way at dispatch and at collect.
func ResolveRef(arg, invocationCwd string) (string, error) {
	if arg == "" {
		return "", errors.New("a job name is required: a name for this project's store, or a directory path")
	}
	if IsPath(arg) {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	if !namePattern.MatchString(arg) {
		return "", fmt.Errorf("job name %q: a name is one path segment of letters, digits, '.', '_' or '-', starting with a letter or digit; use a path for anything else", arg)
	}
	return filepath.Join(DefaultBase(invocationCwd), arg), nil
}

// ErrJobExists reports a reservation that found the directory already there.
var ErrJobExists = errors.New("job already exists")

// Reserve creates dir atomically. An existing directory is a refusal, never
// a rename: the caller chose this name as the address it will collect from,
// and a silent suffix would leave it collecting somebody else's job.
func Reserve(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w: %s", ErrJobExists, dir)
		}
		return err
	}
	return nil
}

// Unreserve removes a directory Reserve created, for a refusal that happens
// before anything was written into it. A directory with content is left
// alone: it is evidence by then.
func Unreserve(dir string) {
	os.Remove(dir)
}

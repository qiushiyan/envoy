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

// namePattern is what one segment of a job name may look like: letters,
// digits, '.', '_' and '-', starting with a letter or digit.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// IsPath reports whether a job argument names a directory outright rather
// than something in the project's store: it starts with '/', '.', or '~'.
func IsPath(arg string) bool {
	return strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "~")
}

// ResolveName turns the name a new job is run as into its directory: one
// segment, in the invoking project's store — or a path, taken as given. The
// project is the one the caller runs in, never the tree a turn is told to
// work in, so a name resolves the same way at dispatch and at collect.
func ResolveName(arg, invocationCwd string) (string, error) {
	if arg == "" {
		return "", errors.New("a job name is required: a name for this project's store, or a directory path")
	}
	if IsPath(arg) {
		return filepath.Abs(arg)
	}
	if !namePattern.MatchString(arg) {
		return "", fmt.Errorf("job name %q: a name is one segment of letters, digits, '.', '_' or '-', starting with a letter or digit; use a path (starting with / or ./) for anything else", arg)
	}
	return filepath.Join(DefaultBase(invocationCwd), arg), nil
}

// ResolveRef turns a reference to an existing job into its directory: a
// name as ResolveName reads it, a fan-out member as name/member, or a path.
func ResolveRef(arg, invocationCwd string) (string, error) {
	if arg == "" {
		return "", errors.New("a job is required: its name in this project's store, or its directory path")
	}
	if IsPath(arg) {
		return filepath.Abs(arg)
	}
	for _, segment := range strings.Split(arg, "/") {
		if !namePattern.MatchString(segment) {
			return "", fmt.Errorf("job %q: a name is one segment of letters, digits, '.', '_' or '-' (a fan-out member is name/member); use a path (starting with / or ./) for anything else", arg)
		}
	}
	return filepath.Join(DefaultBase(invocationCwd), filepath.FromSlash(arg)), nil
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

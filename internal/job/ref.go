package job

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// A job is named by its caller, before it runs, so the caller never has to
// read a coordinate back from a dispatch whose stdout the harness hides.
// The name is an address: it must round-trip exactly, so it is neither
// lowercased nor slugged, and it is reserved by creating the directory —
// creation is the collision check, with no stat-then-create window.
//
// A name addresses its latest generation; a directory is an identity. Callers
// in one long-lived checkout reach for the same names (review-r1, consult-r1),
// so a name whose job has been delivered passes to the next dispatch, which
// runs in <name>+2, then <name>+3. No directory ever moves: every path a
// record or a printed command holds keeps meaning the job it meant.

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
	name, member, _ := strings.Cut(arg, "/")
	dir, _ := Latest(DefaultBase(invocationCwd), name)
	return filepath.Join(dir, filepath.FromSlash(member)), nil
}

// generationSep joins a name to its generation. It is outside namePattern, so
// a caller can never name a generation directly — only its path does — and it
// means nothing to a shell, quoted or not.
const generationSep = "+"

// GenerationDir is the directory of one generation of a name: the bare name
// for the first, <name>+N after it.
func GenerationDir(base, name string, generation int) string {
	if generation <= 1 {
		return filepath.Join(base, name)
	}
	return filepath.Join(base, name+generationSep+strconv.Itoa(generation))
}

// Latest returns the directory a name addresses — its highest generation —
// and that generation's number. Generation 0 means nothing holds the name
// yet, and dir is where its first job would go.
func Latest(base, name string) (dir string, generation int) {
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if g := generationOf(e.Name(), name); g > generation {
			generation = g
		}
	}
	return GenerationDir(base, name, generation), generation
}

// generationOf reads which generation of name an entry is, 0 for none. Only
// the exact spelling GenerationDir writes counts: <name>+1 and <name>+02 are
// somebody else's directories.
func generationOf(entry, name string) int {
	if entry == name {
		return 1
	}
	digits, ok := strings.CutPrefix(entry, name+generationSep)
	if !ok || digits == "" || digits[0] == '0' || strings.Trim(digits, "0123456789") != "" {
		return 0
	}
	g, err := strconv.Atoi(digits)
	if err != nil || g < 2 {
		return 0
	}
	return g
}

// ErrJobExists reports a reservation that found the directory already there.
var ErrJobExists = errors.New("job already exists")

// Reserve creates dir atomically, and an existing directory is an error: the
// primitive never picks another. Which directory a name's next job gets is
// decided by the caller of Reserve, from Latest and what holds that one.
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

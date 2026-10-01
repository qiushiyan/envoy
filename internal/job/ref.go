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
// A name is an address scoped to its caller; a directory is an identity.
// Callers in one checkout reach for the same names (review-r1, consult-r1), so
// every dispatch under a name gets a generation of its own — <name>, <name>+2,
// <name>+3 — and the name means the caller's own newest generation, or the
// newest of anyone's when the caller has none (work picked up from an earlier
// session) or no identity. No directory ever moves: every path a record or a
// printed command holds keeps meaning the job it meant.

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
// name as ResolveName reads it, resolved for this caller; a fan-out member as
// name/member; or a path.
func ResolveRef(arg, invocationCwd, caller string) (string, error) {
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
	addr, err := Resolve(DefaultBase(invocationCwd), name, caller)
	if err != nil {
		return "", err
	}
	return filepath.Join(addr.Dir, filepath.FromSlash(member)), nil
}

// StoreUnreadableError reports a project store whose generations could not be
// listed. It is not "no such job": an unlisted store may hold a newer
// generation than any a guess would land on.
type StoreUnreadableError struct {
	Base string
	Err  error
}

func (e *StoreUnreadableError) Error() string {
	return fmt.Sprintf("the job store at %s could not be read: %s", e.Base, e.Err)
}

func (e *StoreUnreadableError) Unwrap() error { return e.Err }

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

// Address is what a name means to one caller, and where the store stands.
type Address struct {
	// Dir is the job the name addresses: the caller's own newest generation,
	// else the newest of anyone's, else — Newest 0 — where a first job would go.
	Dir string
	// Newest is the highest generation that exists, 0 for none, and NewestDir
	// its directory: what the name means to a caller with no generation of
	// its own. The next dispatch is generation Newest+1 whoever sends it.
	Newest    int
	NewestDir string
}

// Resolve reads what name addresses for caller ("" = no identity). Two things
// it cannot read are errors rather than absences, because either could make
// an older job answer to the name: a store that exists and cannot be listed,
// and — for a caller with an identity — a generation newer than the one
// selected whose record cannot say whose it is. Only a store that does not
// exist yet reads as empty, and only a generation with no record yet reads
// as nobody's.
func Resolve(base, name, caller string) (Address, error) {
	entries, err := os.ReadDir(base)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Address{}, &StoreUnreadableError{Base: base, Err: err}
	}
	newest, own := 0, 0
	unattributed := map[int]error{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		g := generationOf(e.Name(), name)
		if g > newest {
			newest = g
		}
		if g == 0 || caller == "" {
			continue
		}
		owner, err := CallerOf(filepath.Join(base, e.Name()))
		if err != nil {
			unattributed[g] = err
		} else if owner == caller && g > own {
			own = g
		}
	}
	for g, err := range unattributed {
		if g > own {
			return Address{}, &UnattributedError{Name: name, Dir: GenerationDir(base, name, g), Err: err}
		}
	}
	addr := Address{Newest: newest, NewestDir: GenerationDir(base, name, newest)}
	addr.Dir = addr.NewestDir
	if own > 0 {
		addr.Dir = GenerationDir(base, name, own)
	}
	return addr, nil
}

// UnattributedError reports a generation whose record exists and cannot be
// read, so whether it is the caller's newest job under the name is unknown.
type UnattributedError struct {
	Name string
	Dir  string
	Err  error
}

func (e *UnattributedError) Error() string {
	return fmt.Sprintf("the record in %s could not be read: %s", e.Dir, e.Err)
}

func (e *UnattributedError) Unwrap() error { return e.Err }

// CallerOf reads the identity a job was dispatched under. "" with no error
// means the records carry none: a reservation before its first record, a
// record from before callers were recorded, a job run with no harness
// identity. A record that is there and cannot be read is an error, never "".
func CallerOf(dir string) (string, error) {
	path := Workspace{Dir: dir}.MetaPath()
	if IsGroupDir(dir) {
		g, err := ReadGroupFile(GroupWorkspace{Dir: dir}.GroupPath())
		if err != nil {
			return "", err
		}
		return Deref(g.Caller), nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	m, err := ReadMetaFile(path)
	if err != nil {
		return "", err
	}
	return Deref(m.Caller), nil
}

// CallerEnvKeys are the variables a harness exports its session identity in,
// in precedence order: ENVOY_CALLER is the explicit form any harness can set,
// and Claude Code exports its session id on its own.
var CallerEnvKeys = []string{"ENVOY_CALLER", "CLAUDE_CODE_SESSION_ID"}

// CallerFromEnv is the dispatching session's identity as its harness exports
// it, "" when it exports none. It is an observation of the environment, never
// derived: a caller without one gets the unscoped meaning of a name, under
// which holds still protect it and a reused name can still be refused.
func CallerFromEnv() string {
	for _, key := range CallerEnvKeys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
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

// Package gitx shells out to git for the few read-only facts the engine needs.
package gitx

import (
	"fmt"
	"os/exec"
	"strings"
)

// Root returns the repository toplevel for cwd, or "" outside a repo.
func Root(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Head returns the current HEAD sha for cwd, or "" when unavailable.
func Head(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Run executes a git command for display purposes. Failures are returned as a
// printable line rather than an error, matching how collect reports them.
func Run(cwd string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Sprintf("(git %s failed: %s)", strings.Join(args, " "), detail)
	}
	return strings.TrimRight(string(out), "\n")
}

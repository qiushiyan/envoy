package job

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// KV is one ordered progress-line field. A nil value drops the field, matching
// how observation fields that have no reading yet are omitted.
type KV struct {
	K string
	V any
}

var progressSpace = regexp.MustCompile(`\s+`)

// ProgressLog appends runner-owned semantic lines to progress.log. It is a
// convenience view for a human or agent tailing the job, so a write failure
// warns once on Warn and never disturbs the turn.
type ProgressLog struct {
	path   string
	warned bool

	// Warn receives the once-only write-failure warning. Callers embedding
	// the engine set it to their error stream; nil falls back to os.Stderr.
	Warn io.Writer
}

func (w Workspace) Progress() *ProgressLog {
	return &ProgressLog{path: w.ProgressLogPath()}
}

func (p *ProgressLog) Append(state string, fields ...KV) {
	var b strings.Builder
	b.WriteString(ISO(time.Now()))
	b.WriteString(" state=")
	b.WriteString(state)
	for _, f := range fields {
		if f.V == nil {
			continue
		}
		b.WriteString(" ")
		b.WriteString(f.K)
		b.WriteString("=")
		b.WriteString(progressSpace.ReplaceAllString(fmt.Sprint(f.V), "_"))
	}
	b.WriteString("\n")

	f, err := os.OpenFile(p.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, err = f.WriteString(b.String())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil && !p.warned {
		warn := p.Warn
		if warn == nil {
			warn = io.Writer(os.Stderr)
		}
		fmt.Fprintf(warn, "progress log warning: %s\n", err)
		p.warned = true
	}
}

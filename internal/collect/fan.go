package collect

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/proc"
	"github.com/qiushiyan/envoy/internal/prose"
)

// collectGroup prints a whole fan-out: the aggregate first, then one section
// per member, split by member name. The member sections are rendered before
// the header because collecting a member can change its status — reconciling
// an abandoned turn — and an aggregate that disagreed with the sections below
// it would be worse than no aggregate at all. Nothing is stamped until the
// whole block has reached the caller.
func collectGroup(dir, note string, mode Mode, w, errW io.Writer) int {
	fan, err := job.ReadFan(dir)
	if err != nil {
		fmt.Fprintf(errW,
			"collect error: %s could not be read (%s). Each member's job dir under %s is self-contained — collect one directly to see its result.\n",
			job.GroupWorkspace{Dir: dir}.GroupPath(), err, dir)
		return job.ExitUsage
	}
	group := fan.Group

	var sections bytes.Buffer
	statuses := make([]string, 0, len(fan.Members))
	labels := make([]string, 0, len(fan.Members))
	var undelivered []string
	type stampable struct {
		dir  string
		meta *job.Meta
	}
	var stamps []stampable
	resumable := len(fan.Members) > 0
	for _, m := range fan.Members {
		fmt.Fprintf(&sections, "\n=== member %s ===\n", m.Name)
		// A member with no record is the roster's observation, not the
		// member's: it gets its own section here, worded to what the directory
		// shows, rather than the single-turn error a turn with no record earns.
		if errors.Is(m.Err, job.ErrNoRecord) {
			fmt.Fprintf(&sections, "status: %s\nnext: %s\n", prose.FanMemberNoRecord(), prose.FanMemberNoRecordNext(m.Dir))
			statuses = append(statuses, "")
			labels = append(labels, m.Name+" no status")
			resumable = false
			continue
		}
		status := ""
		if m.Err != nil {
			reportUnreadable(m.Dir, m.Err, errW)
			fmt.Fprintf(&sections, "status: %s\nnext: %s\n", prose.FanMemberUnreadable(), prose.FanMemberUnreadableNext(m.Dir))
			resumable = false
		} else {
			r := renderJob(m.Dir, m.Meta, mode, &sections, true)
			status = r.meta.Status
			if r.undelivered {
				undelivered = append(undelivered, m.Name)
			}
			if r.stamp {
				stamps = append(stamps, stampable{m.Dir, r.meta})
			}
			if _, blocked := continuationBlocker(r.meta); blocked {
				resumable = false
			}
		}
		statuses = append(statuses, status)
		label := status
		if label == "" {
			label = "no status"
		}
		labels = append(labels, m.Name+" "+label)
	}

	// Every member runs in the fan-out's one process, so its liveness is the
	// set's: what a stop reaches, and whether a member recorded as running
	// can still be waited on.
	live := proc.RunnerLiveness(dir, group.RunnerPid, group.RunnerLock) == proc.Live
	running := slices.Contains(statuses, job.StatusRunning)

	// The group's closing aggregates delivery alongside status: a member that
	// reports ok while its payload never printed must not be folded into
	// "every result above is usable". A member still recorded as running once
	// the fan-out's process is gone is nothing a wait can end — a wait would
	// return at once and send the caller back here — so the closing defers to
	// the action that member's own section prescribes.
	closing := func() string {
		switch {
		case len(undelivered) > 0:
			return prose.FanUndeliveredResults(undelivered)
		case running && !live:
			return prose.FanRunnerGone()
		}
		return prose.FanCollected(dir, statuses)
	}

	var body bytes.Buffer
	if mode == ModeResultOnly {
		// Result-only keeps the aggregate line and the member split —
		// attribution is the point of a fan-out — and drops the preamble.
		fmt.Fprintf(&body, "status: %s\n", prose.FanStatusLine(statuses))
		body.Write(sections.Bytes())
		if len(undelivered) > 0 || prose.FanStatus(statuses) != prose.FanOK {
			fmt.Fprintf(&body, "\nnext: %s\n", closing())
		}
	} else {
		fmt.Fprintf(&body, "fan-out: %s\n", dir)
		fmt.Fprintf(&body, "status: %s\n", prose.FanStatusLine(statuses))
		fmt.Fprintf(&body, "members: %s\n", strings.Join(labels, " · "))
		// One stop covers the set.
		if live && group.RunnerPid > 0 {
			fmt.Fprintf(&body, "stop: %s\n", prose.StopCommand(group.RunnerPid))
		}
		// The set-level follow-up is offered only when it is provably
		// possible: every member finished and holds a session to continue.
		if resumable {
			fmt.Fprintf(&body, "resume: %s\n", prose.ContinueCommand(dir, group.TimeoutMin))
		}
		// Every member reviewed the same range, so it is reported once here
		// rather than repeated under each of them.
		if group.GitBaseline != nil {
			printGitSinceBaseline(&body, group.Cwd, *group.GitBaseline)
		}
		body.Write(sections.Bytes())
		// A fan-out with a member still recorded as running gets its closing
		// whatever the mode, as a single turn does; a status check of a
		// finished one says that it delivered nothing.
		if mode == ModeStatusOnly && !running {
			fmt.Fprintf(&body, "\nnext: %s\n", prose.StatusOnlyNext(dir))
		} else {
			fmt.Fprintf(&body, "\nnext: %s\n", closing())
		}
	}
	if !deliver(w, errW, withNote(body.Bytes(), note, mode, errW)) {
		return job.ExitInfra
	}
	for _, s := range stamps {
		stampCollected(s.dir, s.meta)
	}
	return 0
}

package envoy

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/qiushiyan/envoy/internal/collect"
	"github.com/qiushiyan/envoy/internal/fan"
	"github.com/qiushiyan/envoy/internal/job"
	"github.com/qiushiyan/envoy/internal/prose"
	"github.com/qiushiyan/envoy/internal/provider"
	"github.com/qiushiyan/envoy/internal/runner"
)

// Roster resolution: what each --with seat spells, resolved into the turns a
// job runs. A cold voice, a continued job and each member of a continued
// fan-out all become one voice and pass one set of roster checks; cardinality
// then selects the flat or fan-out layout and nothing else.

// voice is one turn of the roster before the shared settings are applied:
// what the caller spelled, resolved into the turn it runs.
type voice struct {
	base       string // the name to derive the member address from
	promptFile string // the prompt this voice alone was given; "" = the job's default
	provider   string
	model      string
	effort     string
	session    string // "" = a fresh conversation
	source     string // the job dir whose conversation session continues
	cwd        string // recorded tree, for a continued voice
	baseline   string // recorded anchor, for a continued voice
	allowWrite bool   // recorded write intent, for a continued voice
}

// resolveTurns turns the voices into the turns a job runs, every refusal
// decided before a directory is reserved or a session locked. A fan-out
// reference expands into its members and each one is resolved exactly as a
// member named directly would be, so there is one eligibility rule and one
// set of roster checks whatever the caller spelled. errText is "" exactly
// when the roster is dispatchable.
func resolveTurns(req RunRequest, invocationCwd, caller string, timeoutMin float64) ([]fan.Turn, string) {
	var voices []voice
	for _, seat := range req.With {
		seated, errText := seatVoices(seat, req, invocationCwd, caller)
		if errText != "" {
			return nil, errText
		}
		voices = append(voices, seated...)
	}
	shared, errText := shareRoster(voices, req, timeoutMin)
	if errText != "" {
		return nil, errText
	}
	if shared.cwd == "" {
		shared.cwd = invocationCwd
	}
	shared.cwd = absOrSelf(shared.cwd)

	taken := map[string]bool{}
	turns := make([]fan.Turn, len(voices))
	for i, v := range voices {
		turns[i] = fan.Turn{
			Name: memberName(v.base, taken),
			Options: runner.Options{
				Provider:    v.provider,
				PromptFile:  v.promptFile,
				Cwd:         shared.cwd,
				Baseline:    shared.baseline,
				AllowWrite:  shared.allowWrite,
				ResumedFrom: v.source,
				Caller:      caller,
				TimeoutMin:  timeoutMin,
				Turn: provider.Options{
					Model:        v.model,
					Effort:       v.effort,
					Resume:       v.session,
					MaxBudgetUSD: req.MaxBudgetUSD,
				},
			},
		}
	}
	return turns, ""
}

// seatVoices resolves one --with seat into the voices it seats, each with
// the prompt it is sent: one for a cold voice or a continued turn, every
// member for a fan-out reference, which therefore stands alone.
func seatVoices(seat string, req RunRequest, invocationCwd, caller string) ([]voice, string) {
	spec, promptFile, errText := splitSeat(seat)
	if errText != "" {
		return nil, errText
	}
	if promptFile == "" {
		promptFile = req.PromptFile
	}
	if promptFile == "" {
		return nil, prose.VoiceNeedsPrompt(spec)
	}
	if !strings.HasPrefix(spec, "@") {
		v, err := parseVoice(spec)
		if err != nil {
			return nil, err.Error()
		}
		v.promptFile = promptFile
		return []voice{v}, ""
	}
	ref, err := job.ResolveRef(strings.TrimPrefix(spec, "@"), invocationCwd, caller)
	if err != nil {
		return nil, fmt.Sprintf("--with %s: %s", spec, err)
	}
	if job.IsGroupDir(ref) {
		if len(req.With) > 1 {
			return nil, prose.GroupRefMustStandAlone(ref, fanCandidates(ref))
		}
		return fanMembers(ref, promptFile)
	}
	meta, blocker, err := collect.Inspect(ref)
	if err != nil {
		return nil, prose.ContinueNoTurn(ref, err)
	}
	if blocker != "" {
		return nil, prose.ContinueBlocked(ref, blocker)
	}
	v := continuedVoice(ref, meta, "")
	v.promptFile = promptFile
	return []voice{v}, ""
}

// roster is what every turn of a job shares: one tree, one anchor, and one
// write intent.
type roster struct {
	cwd, baseline string // "" = not chosen and not inherited
	allowWrite    bool
}

// shareRoster applies the roster checks: one conversation once; one tree and
// one anchor across every continued voice unless the caller chose them; and
// write intent only on a single voice. Write intent is checked after
// expansion: a roster is read-only unless it is one voice, and a continued
// write conversation keeps its intent only by continuing alone.
func shareRoster(voices []voice, req RunRequest, timeoutMin float64) (roster, string) {
	shared := roster{cwd: req.Cwd, baseline: req.Baseline}
	sessions := map[string]string{}
	inheritedCwdFrom, inheritedBaselineFrom := "", ""
	var writeSources []string
	for _, v := range voices {
		if v.session == "" {
			continue
		}
		if prev, dup := sessions[v.session]; dup {
			return roster{}, prose.DuplicateConversation(v.session, prev, v.source)
		}
		sessions[v.session] = v.source
		if v.allowWrite {
			writeSources = append(writeSources, v.source)
		}
		if req.Cwd == "" && v.cwd != "" {
			if shared.cwd == "" {
				shared.cwd, inheritedCwdFrom = v.cwd, v.source
			} else if v.cwd != shared.cwd {
				return roster{}, prose.CwdMix(inheritedCwdFrom, shared.cwd, v.source, v.cwd)
			}
		}
		if req.Baseline == "" && v.baseline != "" {
			if shared.baseline == "" {
				shared.baseline, inheritedBaselineFrom = v.baseline, v.source
			} else if v.baseline != shared.baseline {
				return roster{}, prose.BaselineMix(inheritedBaselineFrom, shared.baseline, v.source, v.baseline)
			}
		}
	}
	switch {
	case len(voices) == 1:
		shared.allowWrite = req.AllowWrite || len(writeSources) == 1
	case req.AllowWrite:
		return roster{}, prose.AllowWriteNeedsOneVoice()
	case len(writeSources) > 0:
		return roster{}, prose.WriteSourceInRoster(writeSources[0], timeoutMin)
	}
	return shared, ""
}

// continuedVoice is the voice that continues a finished job's conversation,
// from the record collect.Inspect found continuable. name presets the member
// address (a fan-out's member keeps its identity across rounds); "" derives it.
func continuedVoice(dir string, meta *job.Meta, name string) voice {
	model := job.Deref(meta.Model)
	if name == "" {
		name = voiceBase(meta.Provider, model)
	}
	return voice{
		base:       name,
		provider:   meta.Provider,
		model:      model,
		effort:     job.Deref(meta.Effort),
		session:    *meta.SessionID,
		source:     dir,
		cwd:        meta.Cwd,
		baseline:   job.Deref(meta.GitBaseline),
		allowWrite: meta.AllowWrite,
	}
}

// splitSeat separates what the caller spelled into the voice and, when one
// was attached with '=', that voice's own prompt file. The first '=' splits:
// no provider, model, effort or job name contains one, and the prompt path
// to its right may. A job named by a directory path is the one form that
// could carry '=' legitimately, and there it is refused rather than guessed
// at — but only when such a directory actually exists, since otherwise the
// spelling can only have meant an attachment.
func splitSeat(seat string) (spec, promptFile, errText string) {
	i := strings.IndexByte(seat, '=')
	if i < 0 {
		return seat, "", ""
	}
	if strings.HasPrefix(seat, "@") {
		if whole := strings.TrimPrefix(seat, "@"); job.IsPath(whole) {
			if info, err := os.Stat(whole); err == nil && info.IsDir() {
				return "", "", prose.JobPathHasEquals(seat)
			}
		}
	}
	spec, promptFile = seat[:i], seat[i+1:]
	if promptFile == "" {
		return "", "", fmt.Sprintf("--with %s: give the prompt file after '=' (--with %s<file>), or drop the '=' to use --prompt-file", seat, seat)
	}
	return spec, promptFile, ""
}

// fanMember is one member of a fan-out judged by the one eligibility
// definition: the candidate prose words, and the record a continuation of it
// inherits from — nil when the candidate is blocked.
type fanMember struct {
	prose.FanMemberCandidate
	meta *job.Meta
}

// inspectFan reads a fan-out's members in roster order, each through
// collect.Continuable, so every surface that offers or dispatches a member
// agrees on which ones can continue.
func inspectFan(dir string) ([]fanMember, error) {
	fan, err := job.ReadFan(dir)
	if err != nil {
		return nil, err
	}
	members := make([]fanMember, len(fan.Members))
	for i, m := range fan.Members {
		c := prose.FanMemberCandidate{Name: m.Name, Dir: m.Dir}
		meta, blocker, err := collect.Continuable(m.Meta, m.Err)
		switch {
		case err != nil:
			c.Kind, c.Detail = prose.BlockerUnreadableMeta, err.Error()
		case blocker != "":
			c.Kind = blocker
		}
		members[i] = fanMember{c, meta}
	}
	return members, nil
}

// fanMembers expands a fan-out reference into its members as continued
// voices, each keeping the name it had and each sent promptFile — the round's
// one NEW prompt. The set is whole or refused: any member that cannot continue
// refuses the round rather than being silently left out of it.
func fanMembers(dir, promptFile string) ([]voice, string) {
	members, err := inspectFan(dir)
	if err != nil {
		return nil, prose.FanContinueUnreadableManifest(dir, err)
	}
	if len(members) == 0 {
		return nil, prose.FanContinueEmptyManifest(dir)
	}
	var voices []voice
	var reasons []string
	for _, m := range members {
		if m.Kind != "" {
			reasons = append(reasons, prose.FanResumeBlockerLine(m.Name, m.Kind, m.Detail))
			continue
		}
		v := continuedVoice(m.Dir, m.meta, m.Name)
		v.promptFile = promptFile
		voices = append(voices, v)
	}
	if len(reasons) > 0 {
		return nil, prose.FanContinueBlocked(dir, reasons)
	}
	return voices, ""
}

// fanCandidates lists a fan-out's members in roster order, so a refusal never
// offers a member dispatch would refuse.
func fanCandidates(dir string) []prose.FanMemberCandidate {
	members, err := inspectFan(dir)
	if err != nil {
		return nil
	}
	candidates := make([]prose.FanMemberCandidate, len(members))
	for i, m := range members {
		candidates[i] = m.FanMemberCandidate
	}
	return candidates
}

// parseVoice reads one cold voice. The colon form keeps a voice's settings
// unambiguously attached to it — no provider name, model name or effort value
// contains a colon — where repeated flags could not say which voice they
// belonged to.
func parseVoice(spec string) (voice, error) {
	parts := strings.Split(spec, ":")
	if len(parts) > 3 {
		return voice{}, fmt.Errorf("--with '%s' has too many fields; the form is provider[:model[:effort]], for example claude:opus:high", spec)
	}
	v := voice{provider: parts[0]}
	if len(parts) > 1 {
		v.model = parts[1]
	}
	if len(parts) > 2 {
		v.effort = parts[2]
	}
	if !provider.Known(v.provider) {
		return voice{}, fmt.Errorf("--with '%s' must name provider %s, got '%s'", spec, strings.Join(provider.Names(), " or "), v.provider)
	}
	if err := provider.ValidateEffort(v.provider, v.effort); err != nil {
		return voice{}, fmt.Errorf("--with '%s': %s", spec, err)
	}
	v.base = voiceBase(v.provider, v.model)
	return v, nil
}

// voiceBase is the member address a voice derives from what distinguishes
// it: the provider, plus the model when one was named.
func voiceBase(providerName, model string) string {
	if model != "" {
		return providerName + "-" + model
	}
	return providerName
}

// memberNameUnsafe is the member-name rule: lowercase, path-safe, and free of
// leading or trailing separators, because the name is both a directory and the
// label every group line uses for that member.
var memberNameUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// memberName allocates a member's address from its base — the provider plus the
// model when one was named, or a preset name carried over from an earlier
// round — against the addresses already taken. A repeat is numbered, and the
// numbered form is checked against the taken set too: a base that happens to
// spell a sibling's numbered name must not land on that sibling's directory.
func memberName(base string, taken map[string]bool) string {
	base = strings.Trim(memberNameUnsafe.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if base == "" {
		base = "member"
	}
	if len(base) > 40 {
		base = base[:40]
	}
	name := base
	for n := 2; taken[name]; n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	taken[name] = true
	return name
}

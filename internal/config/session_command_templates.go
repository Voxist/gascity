package config

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// SessionCommandTemplateContext holds the placeholders available to the
// session_setup, pre_start and session_live command templates. cmd/gc builds
// one per session and expands with ExpandAgentSessionCommands; config
// validation expands the same templates against sample contexts so that a
// template accepted at load expands at session start.
type SessionCommandTemplateContext struct {
	Session   string // session name (tmux session for tmux runtimes)
	Agent     string // qualified agent name
	AgentBase string // unqualified agent name or pool instance name
	Rig       string // rig name (empty for city-scoped agents)
	RigRoot   string // absolute path to the rig root (empty for city-scoped agents)
	CityRoot  string // city directory path
	CityName  string // workspace name
	WorkDir   string // agent working directory
	ConfigDir string // directory the agent config was defined in
	// DefaultBranch mirrors workdir.PathContext.DefaultBranch: the rig's
	// configured mainline branch, empty for city-scoped agents and for rigs
	// with no default_branch. Configured value only — prompts'
	// {{.DefaultBranch}} additionally falls back to a live origin/HEAD probe,
	// but setup-command expansion runs on reconciler hot paths and must not
	// spawn git. Setup scripts should keep their own probe fallback.
	DefaultBranch string
}

// sessionCommandTemplateSamples are the contexts validation expands every
// template against. Values are one character so that any length-dependent
// operation (slice, index) that validation accepts also succeeds for the
// shortest legal runtime value; the second sample blanks the two fields
// that are legitimately empty at runtime so both arms of an {{if .Rig}} run.
// A placeholder hidden behind a comparison that neither sample satisfies is
// caught at session start, which fails closed on the same expander.
var sessionCommandTemplateSamples = []SessionCommandTemplateContext{
	{Session: "s", Agent: "a", AgentBase: "b", Rig: "r", RigRoot: "/r", CityRoot: "/c", CityName: "n", WorkDir: "/w", ConfigDir: "/d", DefaultBranch: "m"},
	{Session: "s", Agent: "a", AgentBase: "b", Rig: "", RigRoot: "", CityRoot: "/c", CityName: "n", WorkDir: "/w", ConfigDir: "/d", DefaultBranch: ""},
}

// sessionCommandTemplatePlaceholders is the "{{.Session}}, {{.Agent}}, ..."
// list printed in validation errors.
const sessionCommandTemplatePlaceholders = "{{.Session}}, {{.Agent}}, {{.AgentBase}}, {{.Rig}}, {{.RigRoot}}, {{.CityRoot}}, {{.CityName}}, {{.WorkDir}}, {{.ConfigDir}}, {{.DefaultBranch}}"

// ExpandSessionCommandTemplate expands one command string against ctx.
// field and index name the config entry in the error. A command without
// "{{" is returned unchanged.
func ExpandSessionCommandTemplate(raw string, ctx SessionCommandTemplateContext, field string, index int) (string, error) {
	if !strings.Contains(raw, "{{") {
		return raw, nil
	}
	tmpl, err := template.New(field).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s[%d] %q: parsing template: %w", field, index, raw, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("%s[%d] %q: expanding template: %w", field, index, raw, err)
	}
	return buf.String(), nil
}

// ExpandSessionCommandTemplates expands every command in cmds. It fails
// closed: one entry that does not parse or expand yields an error and no
// commands, because a command that still carries template placeholders
// must never reach sh.
func ExpandSessionCommandTemplates(cmds []string, ctx SessionCommandTemplateContext, field string) ([]string, error) {
	if len(cmds) == 0 {
		return nil, nil
	}
	result := make([]string, len(cmds))
	for i, raw := range cmds {
		expanded, err := ExpandSessionCommandTemplate(raw, ctx, field, i)
		if err != nil {
			return nil, err
		}
		result[i] = expanded
	}
	return result, nil
}

// ExpandedSessionCommands is the result of ExpandAgentSessionCommands.
type ExpandedSessionCommands struct {
	SessionSetup []string
	PreStart     []string
	// SessionLive holds only the entries that expanded; Skipped reports the
	// ones dropped, in order.
	SessionLive []string
	Skipped     []error
}

// ExpandAgentSessionCommands expands an agent's session_setup, pre_start and
// session_live commands against ctx with the policy each field carries:
// session_setup and pre_start fail closed (they run scripts against the
// filesystem, and a literal "{{.AgentBase}}" reaching one of them once
// minted a git worktree of that name); session_live is cosmetic, so an
// entry that does not expand is dropped and reported in Skipped while its
// neighbors still run. Config validation and every runtime expansion site
// go through this one function so the policy cannot diverge.
func ExpandAgentSessionCommands(a Agent, ctx SessionCommandTemplateContext) (ExpandedSessionCommands, error) {
	var out ExpandedSessionCommands
	var err error
	if out.SessionSetup, err = ExpandSessionCommandTemplates(a.SessionSetup, ctx, "session_setup"); err != nil {
		return ExpandedSessionCommands{}, err
	}
	if out.PreStart, err = ExpandSessionCommandTemplates(a.PreStart, ctx, "pre_start"); err != nil {
		return ExpandedSessionCommands{}, err
	}
	for i, raw := range a.SessionLive {
		expanded, err := ExpandSessionCommandTemplate(raw, ctx, "session_live", i)
		if err != nil {
			out.Skipped = append(out.Skipped, err)
			continue
		}
		out.SessionLive = append(out.SessionLive, expanded)
	}
	return out, nil
}

func hasTemplatePlaceholder(cmds ...[]string) bool {
	for _, list := range cmds {
		for _, c := range list {
			if strings.Contains(c, "{{") {
				return true
			}
		}
	}
	return false
}

func sessionCommandTemplateHint(a Agent, err error) string {
	return fmt.Sprintf("agent %q: %v (available placeholders: %s; escape literal braces as {{\"{{\"}})", a.QualifiedName(), err, sessionCommandTemplatePlaceholders)
}

// validateSessionCommandTemplates rejects, at config load, a session_setup
// or pre_start template that would fail to expand at session start. Those
// fields run scripts against the filesystem, so a bad template must be
// refused once by gc start / reload / doctor, for the whole city like every
// other ValidateAgents error, rather than fail on every reconciler tick,
// where an unresolvable agent is dropped from the desired set and its
// running sessions are drained.
func validateSessionCommandTemplates(a Agent) error {
	if !hasTemplatePlaceholder(a.SessionSetup, a.PreStart) {
		return nil
	}
	for _, sample := range sessionCommandTemplateSamples {
		if _, err := ExpandAgentSessionCommands(a, sample); err != nil {
			return fmt.Errorf("%s", sessionCommandTemplateHint(a, err))
		}
	}
	return nil
}

// sessionLiveTemplateWarningPrefix marks the load-time warning for a
// session_live entry that will be skipped, so warning filters can keep it
// non-fatal in strict mode and visible in supervisor-managed loads.
const sessionLiveTemplateWarningPrefix = "session_live template will be skipped: "

// IsSessionLiveTemplateWarning reports whether a config warning is the
// non-fatal notice for a session_live template that will be skipped at
// session start.
func IsSessionLiveTemplateWarning(warning string) bool {
	return strings.Contains(warning, sessionLiveTemplateWarningPrefix)
}

// warnSessionLiveTemplates returns one warning per session_live entry that
// would not expand at session start. The entry is skipped at runtime, so
// the city still starts and running sessions can still be managed.
func warnSessionLiveTemplates(a Agent) []string {
	if !hasTemplatePlaceholder(a.SessionLive) {
		return nil
	}
	seen := map[string]bool{}
	var warnings []string
	for _, sample := range sessionCommandTemplateSamples {
		// session_setup/pre_start errors are reported by ValidateAgents; a
		// failing sample here would only repeat them, so ignore the error.
		out, _ := ExpandAgentSessionCommands(Agent{SessionLive: a.SessionLive, Name: a.Name, Dir: a.Dir, BindingName: a.BindingName}, sample)
		for _, skipErr := range out.Skipped {
			w := sessionLiveTemplateWarningPrefix + sessionCommandTemplateHint(a, skipErr)
			if !seen[w] {
				seen[w] = true
				warnings = append(warnings, w)
			}
		}
	}
	return warnings
}

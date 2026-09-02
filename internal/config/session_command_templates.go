package config

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"text/template"
)

// SessionCommandTemplateContext holds the placeholders available to the
// session_setup, pre_start and session_live command templates. cmd/gc builds
// one per session and expands with ExpandSessionCommandTemplates; config
// validation executes the same templates against sample contexts so that a
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
}

// sessionCommandTemplateFieldsMayBeEmpty lists the context fields that are
// legitimately empty at session start. Validation blanks only these in its
// second sample, so a template that always succeeds at runtime (for example
// one that slices .Session) is not rejected for an emptiness that never
// happens.
var sessionCommandTemplateFieldsMayBeEmpty = map[string]bool{"Rig": true, "RigRoot": true}

// sessionCommandTemplatePlaceholders is the "{{.Session}}, {{.Agent}}, ..."
// list printed in validation errors, built once from the context type.
var sessionCommandTemplatePlaceholders = func() string {
	rt := reflect.TypeOf(SessionCommandTemplateContext{})
	parts := make([]string, rt.NumField())
	for i := range parts {
		parts[i] = "{{." + rt.Field(i).Name + "}}"
	}
	return strings.Join(parts, ", ")
}()

// ExpandSessionCommandTemplates expands Go text/template placeholders in the
// given command strings. field names the config field for error messages
// (session_setup, pre_start, session_live).
//
// It fails closed: a command whose template does not parse or does not
// expand against ctx yields an error and no commands, because a command
// that still carries template placeholders must never reach sh. Commands
// without "{{" are returned unchanged.
func ExpandSessionCommandTemplates(cmds []string, ctx SessionCommandTemplateContext, field string) ([]string, error) {
	if len(cmds) == 0 {
		return nil, nil
	}
	result := make([]string, len(cmds))
	for i, raw := range cmds {
		expanded, err := expandSessionCommandTemplate(raw, ctx, field, i)
		if err != nil {
			return nil, err
		}
		result[i] = expanded
	}
	return result, nil
}

// ExpandSessionCommandTemplatesLenient expands each command independently:
// entries that expand are returned, entries that do not are dropped and
// reported in skipped. It is the policy for cosmetic fields (session_live)
// where one bad entry must not disable its neighbors, such as pack-global
// theming, nor block managing a running session.
func ExpandSessionCommandTemplatesLenient(cmds []string, ctx SessionCommandTemplateContext, field string) (expanded []string, skipped []error) {
	for i, raw := range cmds {
		out, err := expandSessionCommandTemplate(raw, ctx, field, i)
		if err != nil {
			skipped = append(skipped, err)
			continue
		}
		expanded = append(expanded, out)
	}
	return expanded, skipped
}

func expandSessionCommandTemplate(raw string, ctx SessionCommandTemplateContext, field string, i int) (string, error) {
	if !strings.Contains(raw, "{{") {
		return raw, nil
	}
	tmpl, err := template.New(field).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s[%d] %q: parsing template: %w", field, i, raw, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("%s[%d] %q: expanding template: %w", field, i, raw, err)
	}
	return buf.String(), nil
}

// sessionCommandTemplateSamples returns the contexts validation expands
// every template against: one with every field populated and one with the
// fields that may be empty at runtime blanked, so both arms of an
// {{if .Rig}} run. It returns an error rather than panicking if the context
// type ever gains a non-string field.
func sessionCommandTemplateSamples() ([]SessionCommandTemplateContext, error) {
	populated := SessionCommandTemplateContext{}
	rv := reflect.ValueOf(&populated).Elem()
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		if f.Kind() != reflect.String || !f.CanSet() {
			return nil, fmt.Errorf("SessionCommandTemplateContext.%s is not a settable string field; validation samples cannot be built", rv.Type().Field(i).Name)
		}
		f.SetString("sample-" + rv.Type().Field(i).Name)
	}
	partial := populated
	pv := reflect.ValueOf(&partial).Elem()
	for i := 0; i < pv.NumField(); i++ {
		if sessionCommandTemplateFieldsMayBeEmpty[pv.Type().Field(i).Name] {
			pv.Field(i).SetString("")
		}
	}
	return []SessionCommandTemplateContext{populated, partial}, nil
}

// validateSessionCommandTemplateField expands every entry of one field
// against the validation samples and returns the first failure, annotated
// with the placeholder list and the {{"{{"}} escape for literal braces meant
// for another tool (docker --format, kubectl -o go-template, gh --template).
//
// Coverage is the populated and the blanked-optional-fields samples; a
// placeholder hidden behind a comparison that neither sample satisfies is
// still caught at session start, which fails closed on the same expander.
func validateSessionCommandTemplateField(a Agent, field string, cmds []string) error {
	samples, err := sessionCommandTemplateSamples()
	if err != nil {
		return err
	}
	for _, sample := range samples {
		if _, err := ExpandSessionCommandTemplates(cmds, sample, field); err != nil {
			return fmt.Errorf("agent %q: %w (available placeholders: %s; escape literal braces as {{\"{{\"}})", a.QualifiedName(), err, sessionCommandTemplatePlaceholders)
		}
	}
	return nil
}

// validateSessionCommandTemplates rejects, at config load, a session_setup
// or pre_start template that would fail to expand at session start. Those
// fields run scripts against the filesystem, so a bad template must be
// refused once by gc start / reload / doctor rather than fail on every
// reconciler tick, where an unresolvable agent is dropped from the desired
// set and its running sessions are drained. session_live is cosmetic and is
// only warned about (see warnSessionLiveTemplates).
func validateSessionCommandTemplates(a Agent) error {
	if err := validateSessionCommandTemplateField(a, "session_setup", a.SessionSetup); err != nil {
		return err
	}
	return validateSessionCommandTemplateField(a, "pre_start", a.PreStart)
}

// warnSessionLiveTemplates returns a warning for a session_live template
// that would not expand at session start. The entry is skipped at runtime,
// so the city still starts and running sessions can still be managed.
func warnSessionLiveTemplates(a Agent) string {
	if err := validateSessionCommandTemplateField(a, "session_live", a.SessionLive); err != nil {
		return err.Error() + " (entry will be skipped at session start)"
	}
	return ""
}

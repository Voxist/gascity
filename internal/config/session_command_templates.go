package config

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"text/template"
)

// SessionCommandTemplateContext mirrors cmd/gc's SessionSetupContext: the
// placeholders available to session_setup, pre_start and session_live
// templates. cmd/gc keeps the two structs in sync with a reflection test, so
// adding a field there without adding it here fails the build. Config
// validation executes every template against two instances of this struct
// (all fields set, all fields empty) so that both arms of a conditional run.
type SessionCommandTemplateContext struct {
	Session   string
	Agent     string
	AgentBase string
	Rig       string
	RigRoot   string
	CityRoot  string
	CityName  string
	WorkDir   string
	ConfigDir string
}

// SessionCommandTemplateFields lists the placeholder names, derived from
// SessionCommandTemplateContext so there is one source of truth.
var SessionCommandTemplateFields = sessionCommandTemplateFieldNames()

func sessionCommandTemplateFieldNames() []string {
	rt := reflect.TypeOf(SessionCommandTemplateContext{})
	names := make([]string, rt.NumField())
	for i := range names {
		names[i] = rt.Field(i).Name
	}
	return names
}

// sessionCommandTemplateSamples are the contexts every template is executed
// against at validation time. The populated one exercises the "truthy" arm of
// {{if .X}} / {{with .X}}; the zero one exercises the else arm.
func sessionCommandTemplateSamples() []SessionCommandTemplateContext {
	populated := SessionCommandTemplateContext{}
	rv := reflect.ValueOf(&populated).Elem()
	for i := 0; i < rv.NumField(); i++ {
		rv.Field(i).SetString("sample-" + rv.Type().Field(i).Name)
	}
	return []SessionCommandTemplateContext{populated, {}}
}

// sessionCommandTemplateField pairs a config field name with its command
// strings so validation errors name the field the operator wrote.
type sessionCommandTemplateField struct {
	name string
	cmds []string
}

// validateSessionCommandTemplates rejects, at config load, any command
// template that would fail to expand at session start: a malformed template,
// an unknown placeholder, a field lookup on a string ({{.Session.Foo}},
// {{$.Nope}}), an undefined {{template}}, or a {{range}} over a scalar.
// Session start fails closed on the same condition (a command that still
// carries "{{" must never reach sh, ga-iwz7u); catching it here means a typo
// is refused by gc start / reload / doctor instead of surfacing tick after
// tick in the reconciler, where an un-resolvable agent is dropped from the
// desired set and its running sessions are drained.
//
// Validation executes the template rather than inspecting its parse tree, so
// it is exactly as strict as expansion. Literal braces meant for another tool
// (docker --format, kubectl -o go-template, gh --template) must be escaped
// as {{"{{"}}.
func validateSessionCommandTemplates(a Agent) error {
	fields := []sessionCommandTemplateField{
		{"session_setup", a.SessionSetup},
		{"pre_start", a.PreStart},
		{"session_live", a.SessionLive},
	}
	samples := sessionCommandTemplateSamples()
	for _, f := range fields {
		for i, raw := range f.cmds {
			if !strings.Contains(raw, "{{") {
				continue
			}
			tmpl, err := template.New(f.name).Parse(raw)
			if err != nil {
				return fmt.Errorf("agent %q: %s[%d] %q: malformed template: %w (available placeholders: %s; escape literal braces as {{\"{{\"}})",
					a.QualifiedName(), f.name, i, raw, err, availableSessionCommandTemplateFields())
			}
			for _, sample := range samples {
				if err := tmpl.Execute(io.Discard, sample); err != nil {
					return fmt.Errorf("agent %q: %s[%d] %q: template cannot be expanded: %w (available placeholders: %s; escape literal braces as {{\"{{\"}})",
						a.QualifiedName(), f.name, i, raw, err, availableSessionCommandTemplateFields())
				}
			}
		}
	}
	return nil
}

func availableSessionCommandTemplateFields() string {
	parts := make([]string, len(SessionCommandTemplateFields))
	for i, f := range SessionCommandTemplateFields {
		parts[i] = "{{." + f + "}}"
	}
	return strings.Join(parts, ", ")
}

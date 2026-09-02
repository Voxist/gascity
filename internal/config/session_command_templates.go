package config

import (
	"fmt"
	"strings"
	"text/template"
	"text/template/parse"
)

// SessionCommandTemplateFields is the set of placeholders available to the
// session_setup, pre_start and session_live templates (and to a provider
// command that carries placeholders): the fields of
// cmd/gc's SessionSetupContext. cmd/gc keeps the two in sync with a reflection
// test, so adding a field there without listing it here fails the build.
var SessionCommandTemplateFields = []string{
	"Session", "Agent", "AgentBase", "Rig", "RigRoot", "CityRoot", "CityName", "WorkDir", "ConfigDir",
}

// sessionCommandTemplateField pairs a config field name with its command
// strings so validation errors name the field the operator wrote.
type sessionCommandTemplateField struct {
	name string
	cmds []string
}

// validateSessionCommandTemplates rejects, at config load, any command
// template that would fail to expand at session start: a malformed template
// or a placeholder that is not one of SessionCommandTemplateFields. Session
// start fails closed on the same condition (a command that still carries
// "{{" must never reach sh, ga-iwz7u); catching it here means a typo is
// refused by gc start / reload / doctor instead of surfacing tick after tick
// in the reconciler, where an un-resolvable agent is dropped from the
// desired set and its running sessions are drained.
//
// Literal braces meant for another tool (docker --format, kubectl -o
// go-template, gh --template) must be escaped as {{"{{"}}.
func validateSessionCommandTemplates(a Agent) error {
	fields := []sessionCommandTemplateField{
		{"session_setup", a.SessionSetup},
		{"pre_start", a.PreStart},
		{"session_live", a.SessionLive},
	}
	for _, f := range fields {
		for i, raw := range f.cmds {
			if !strings.Contains(raw, "{{") {
				continue
			}
			tmpl, err := template.New(f.name).Parse(raw)
			if err != nil {
				return fmt.Errorf("agent %q: %s[%d] %q: malformed template: %w (escape literal braces as {{\"{{\"}})", a.QualifiedName(), f.name, i, raw, err)
			}
			if bad := firstUnknownTemplateField(tmpl.Root); bad != "" {
				return fmt.Errorf("agent %q: %s[%d] %q: unknown placeholder {{%s}}; available: %s (escape literal braces as {{\"{{\"}})",
					a.QualifiedName(), f.name, i, raw, bad, availableSessionCommandTemplateFields())
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

// firstUnknownTemplateField walks a parsed template and returns the first
// field reference (".Foo" or ".Foo.Bar") whose leading identifier is not an
// allowed SessionSetupContext field, or "" when every reference is allowed.
func firstUnknownTemplateField(node parse.Node) string {
	allowed := make(map[string]bool, len(SessionCommandTemplateFields))
	for _, f := range SessionCommandTemplateFields {
		allowed[f] = true
	}
	var bad string
	var walk func(parse.Node)
	walk = func(n parse.Node) {
		if n == nil || bad != "" {
			return
		}
		switch n := n.(type) {
		case *parse.ListNode:
			if n == nil {
				return
			}
			for _, c := range n.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			walk(n.Pipe)
		case *parse.PipeNode:
			if n == nil {
				return
			}
			for _, c := range n.Cmds {
				walk(c)
			}
		case *parse.CommandNode:
			for _, arg := range n.Args {
				walk(arg)
			}
		case *parse.FieldNode:
			if len(n.Ident) > 0 && !allowed[n.Ident[0]] {
				bad = n.String()
			}
		case *parse.ChainNode:
			walk(n.Node)
		case *parse.IfNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.RangeNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.WithNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.TemplateNode:
			walk(n.Pipe)
		}
	}
	walk(node)
	return bad
}

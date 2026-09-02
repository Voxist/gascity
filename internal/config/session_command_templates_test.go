package config

import (
	"reflect"
	"strings"
	"testing"
)

func sampleSessionCommandContext() SessionCommandTemplateContext {
	return SessionCommandTemplateContext{
		Session: "hw--polecat", Agent: "hw/polecat", AgentBase: "polecat",
		Rig: "hello-world", RigRoot: "/repos/hello-world", CityRoot: "/city",
		CityName: "bl", WorkDir: "/city/.gc/worktrees/polecat", ConfigDir: "/city/packs/gastown",
	}
}

// The validation samples and the placeholder list are hand-written; keep
// them complete against the context type.
func TestSessionCommandTemplateSamplesCoverEveryField(t *testing.T) {
	rt := reflect.TypeOf(SessionCommandTemplateContext{})
	populated := reflect.ValueOf(sessionCommandTemplateSamples[0])
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.String {
			t.Errorf("field %s is %s; every placeholder must be a string", f.Name, f.Type)
		}
		if populated.Field(i).String() == "" {
			t.Errorf("populated sample leaves %s empty", f.Name)
		}
		if !strings.Contains(sessionCommandTemplatePlaceholders, "{{."+f.Name+"}}") {
			t.Errorf("placeholder list omits {{.%s}}", f.Name)
		}
	}
}

func TestExpandSessionCommandTemplates_AllVariables(t *testing.T) {
	got, err := ExpandSessionCommandTemplates([]string{
		"plain",
		"echo {{.Session}} {{.Agent}} {{.AgentBase}} {{.Rig}} {{.RigRoot}} {{.CityRoot}} {{.CityName}} {{.WorkDir}} {{.ConfigDir}}",
	}, sampleSessionCommandContext(), "session_setup")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plain", "echo hw--polecat hw/polecat polecat hello-world /repos/hello-world /city bl /city/.gc/worktrees/polecat /city/packs/gastown"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExpandSessionCommandTemplates_Empty(t *testing.T) {
	for _, cmds := range [][]string{nil, {}} {
		got, err := ExpandSessionCommandTemplates(cmds, SessionCommandTemplateContext{}, "pre_start")
		if err != nil || got != nil {
			t.Errorf("(%v) = %v, %v; want nil, nil", cmds, got, err)
		}
	}
}

// A malformed entry or an unknown placeholder must poison the whole list:
// nothing may reach sh with a raw "{{" in it, not even valid neighbors.
func TestExpandSessionCommandTemplates_FailsClosed(t *testing.T) {
	cases := []struct{ name, bad, want string }{
		{"parse failure", "tmux {{.BadSyntax", "parsing template"},
		{"unknown field", "worktree-setup.sh {{.RigRoot}} {{.WorkDir}} {{.NoSuchField}} --sync", "NoSuchField"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmds := []string{"tmux {{.Session}}", tc.bad, "tmux {{.Session}} ok"}
			got, err := ExpandSessionCommandTemplates(cmds, sampleSessionCommandContext(), "pre_start")
			if err == nil {
				t.Fatalf("no error for %q", tc.bad)
			}
			if got != nil {
				t.Errorf("got %v, want nil on error", got)
			}
			for _, want := range []string{tc.want, "pre_start[1]"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestExpandAgentSessionCommands_PolicyPerField(t *testing.T) {
	a := Agent{
		Name:         "w",
		SessionSetup: []string{"echo {{.Session}}"},
		PreStart:     []string{"setup {{.WorkDir}}"},
		SessionLive:  []string{"tmux set -t {{.Session}} a", "tmux set -g x '{{.Nope}}'", "tmux set -t {{.Session}} b"},
	}
	out, err := ExpandAgentSessionCommands(a, sampleSessionCommandContext())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.SessionSetup, []string{"echo hw--polecat"}) || !reflect.DeepEqual(out.PreStart, []string{"setup /city/.gc/worktrees/polecat"}) {
		t.Errorf("strict fields = %q / %q", out.SessionSetup, out.PreStart)
	}
	if !reflect.DeepEqual(out.SessionLive, []string{"tmux set -t hw--polecat a", "tmux set -t hw--polecat b"}) {
		t.Errorf("SessionLive = %q, want only the good entries", out.SessionLive)
	}
	if len(out.Skipped) != 1 || !strings.Contains(out.Skipped[0].Error(), "session_live[1]") {
		t.Errorf("Skipped = %v", out.Skipped)
	}

	a.PreStart = []string{"setup {{.Typo}}"}
	if _, err := ExpandAgentSessionCommands(a, sampleSessionCommandContext()); err == nil || !strings.Contains(err.Error(), "pre_start[0]") {
		t.Errorf("pre_start typo: err = %v, want fail closed", err)
	}
}

func TestValidateAgentsRejectsUnexpandableSessionTemplate(t *testing.T) {
	cases := []struct {
		name  string
		agent Agent
		want  string
	}{
		{"pre_start typo", Agent{Name: "w", PreStart: []string{"worktree-setup.sh {{.RigRoot}} {{.WorkDir}} {{.AgentBas}} --sync"}}, "pre_start[0]"},
		{"session_setup typo", Agent{Name: "w", SessionSetup: []string{"ok", "tmux set -t {{.Sesion}} x"}}, "session_setup[1]"},
		{"malformed", Agent{Name: "w", SessionSetup: []string{"tmux {{.BadSyntax"}}, "parsing template"},
		{"field on a string", Agent{Name: "w", PreStart: []string{"x {{.Session.NoSuch}}"}}, "NoSuch"},
		{"root variable", Agent{Name: "w", PreStart: []string{"x {{$.NoSuch}}"}}, "NoSuch"},
		{"undefined template", Agent{Name: "w", PreStart: []string{`x {{template "nope"}}`}}, "nope"},
		{"range over a string", Agent{Name: "w", PreStart: []string{"x {{range .Session}}{{.}}{{end}}"}}, "range"},
		{"variable field", Agent{Name: "w", PreStart: []string{"x {{with $v := .Rig}}{{$v.Foo}}{{end}}"}}, "Foo"},
		{"typo in else arm", Agent{Name: "w", PreStart: []string{"x {{if .Rig}}{{.RigRoot}}{{else}}{{.Bogus}}{{end}}"}}, "Bogus"},
		{"typo in if arm", Agent{Name: "w", PreStart: []string{"x {{if .Rig}}{{.Bogus}}{{else}}{{.CityRoot}}{{end}}"}}, "Bogus"},
		{"docker format unescaped", Agent{Name: "w", SessionSetup: []string{"docker inspect -f '{{.State.Running}}' c"}}, "State"},
		// Samples are one character long, so a slice that needs more is
		// rejected: an agent named "qa" would fail it at session start.
		{"slice longer than shortest legal value", Agent{Name: "w", PreStart: []string{"x {{slice .AgentBase 0 3}}"}}, "slice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAgents([]Agent{tc.agent})
			if err == nil {
				t.Fatalf("ValidateAgents accepted %v", tc.agent)
			}
			for _, want := range []string{tc.want, "available placeholders", `{{"{{"}}`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestValidateAgentsAcceptsExpandableSessionTemplates(t *testing.T) {
	agents := []Agent{{
		Name: "w",
		PreStart: []string{
			"{{.ConfigDir}}/assets/scripts/worktree-setup.sh {{.RigRoot}} {{.WorkDir}} {{.AgentBase}} --sync",
			"echo {{.Session}} {{.Agent}} {{.Rig}} {{.CityRoot}} {{.CityName}}",
			"echo {{.Agent | printf \"%q\"}}",
			"{{if .Rig}}echo rig{{else}}echo city{{end}}",
			"{{with $r := .RigRoot}}cd {{$r}}{{end}}",
			"{{$.AgentBase}} {{.Session | printf \"%s\"}}",
			"echo {{index .Agent 0}} {{slice .Session 0 1}}",
		},
		SessionSetup: []string{"plain command, no template"},
		// Literal braces for another tool, escaped the documented way.
		SessionLive: []string{`docker ps --format '{{"{{"}}.Names}}'`},
	}}
	if err := ValidateAgents(agents); err != nil {
		t.Fatalf("ValidateAgents rejected a valid config: %v", err)
	}
}

// session_live is cosmetic: a bad template is a load-time warning, never a
// hard error, so a pack-global theming typo cannot keep a city from starting.
func TestSessionLiveTemplateTypoIsWarningNotError(t *testing.T) {
	a := Agent{Name: "w", SessionLive: []string{"tmux set -g status-right '{{.AgentName}}'", "tmux set -g x '{{.Other}}'"}}
	if err := ValidateAgents([]Agent{a}); err != nil {
		t.Fatalf("ValidateAgents rejected a session_live typo: %v", err)
	}
	warnings := ValidateSemantics(&City{Agents: []Agent{a}}, "city.toml")
	var hits int
	for _, w := range warnings {
		if IsSessionLiveTemplateWarning(w) && strings.Contains(w, "session_live[") && strings.Contains(w, "city.toml") {
			hits++
		}
	}
	if hits != 2 {
		t.Fatalf("want one marked warning per bad entry, got %d in %q", hits, warnings)
	}
}

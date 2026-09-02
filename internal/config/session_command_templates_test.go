package config

import (
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

func TestExpandSessionCommandTemplates_AllVariables(t *testing.T) {
	got, err := ExpandSessionCommandTemplates([]string{
		"plain",
		"echo {{.Session}} {{.Agent}} {{.AgentBase}} {{.Rig}} {{.RigRoot}} {{.CityRoot}} {{.CityName}} {{.WorkDir}} {{.ConfigDir}}",
	}, sampleSessionCommandContext(), "session_setup")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plain", "echo hw--polecat hw/polecat polecat hello-world /repos/hello-world /city bl /city/.gc/worktrees/polecat /city/packs/gastown"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
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

func TestExpandSessionCommandTemplatesLenient_SkipsOnlyBadEntries(t *testing.T) {
	cmds := []string{"tmux set -t {{.Session}} a", "tmux set -g x '{{.Nope}}'", "tmux set -t {{.Session}} b"}
	got, skipped := ExpandSessionCommandTemplatesLenient(cmds, sampleSessionCommandContext(), "session_live")
	if len(got) != 2 || got[0] != "tmux set -t hw--polecat a" || got[1] != "tmux set -t hw--polecat b" {
		t.Errorf("got %q", got)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Error(), "session_live[1]") {
		t.Errorf("skipped = %v", skipped)
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
			// Session/Agent/WorkDir/CityRoot are never empty at runtime, so
			// operations that need a non-empty string must be accepted.
			"echo {{slice .Session 0 2}} {{index .Agent 0}}",
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
	a := Agent{Name: "w", SessionLive: []string{"tmux set -g status-right '{{.AgentName}}'"}}
	if err := ValidateAgents([]Agent{a}); err != nil {
		t.Fatalf("ValidateAgents rejected a session_live typo: %v", err)
	}
	warnings := ValidateSemantics(&City{Agents: []Agent{a}}, "city.toml")
	var hit bool
	for _, w := range warnings {
		if strings.Contains(w, "session_live[0]") && strings.Contains(w, "AgentName") && strings.Contains(w, "skipped") {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("ValidateSemantics warnings %q do not mention the session_live typo", warnings)
	}
}

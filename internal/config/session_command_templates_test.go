package config

import (
	"strings"
	"testing"
)

func TestValidateAgentsRejectsUnknownSessionTemplatePlaceholder(t *testing.T) {
	cases := []struct {
		name  string
		agent Agent
		want  string
	}{
		{"pre_start", Agent{Name: "w", PreStart: []string{"worktree-setup.sh {{.RigRoot}} {{.WorkDir}} {{.AgentBas}} --sync"}}, "pre_start[0]"},
		{"session_setup", Agent{Name: "w", SessionSetup: []string{"ok", "tmux set -t {{.Sesion}} x"}}, "session_setup[1]"},
		{"session_live", Agent{Name: "w", SessionLive: []string{"tmux set -g status-right '{{.AgentName}}'"}}, "session_live[0]"},
		{"nested field", Agent{Name: "w", PreStart: []string{"x {{.State.Running}}"}}, "{{.State.Running}}"},
		{"inside if", Agent{Name: "w", PreStart: []string{"x {{if .Rig}}{{.RigRoot}}{{else}}{{.Bogus}}{{end}}"}}, "{{.Bogus}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAgents([]Agent{tc.agent})
			if err == nil {
				t.Fatalf("ValidateAgents accepted %v", tc.agent)
			}
			for _, want := range []string{tc.want, "unknown placeholder", `{{"{{"}}`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestValidateAgentsRejectsMalformedSessionTemplate(t *testing.T) {
	err := ValidateAgents([]Agent{{Name: "w", SessionSetup: []string{"tmux {{.BadSyntax"}}})
	if err == nil {
		t.Fatal("ValidateAgents accepted a malformed template")
	}
	if !strings.Contains(err.Error(), "malformed template") || !strings.Contains(err.Error(), "session_setup[0]") {
		t.Errorf("error = %q", err)
	}
}

func TestValidateAgentsAcceptsKnownSessionTemplatePlaceholders(t *testing.T) {
	agents := []Agent{{
		Name: "w",
		PreStart: []string{
			"{{.ConfigDir}}/assets/scripts/worktree-setup.sh {{.RigRoot}} {{.WorkDir}} {{.AgentBase}} --sync",
			"echo {{.Session}} {{.Agent}} {{.Rig}} {{.CityRoot}} {{.CityName}}",
			"echo {{.Agent | printf \"%q\"}}",
			"{{if .Rig}}echo rig{{else}}echo city{{end}}",
		},
		SessionSetup: []string{"plain command, no template"},
		// Literal braces for another tool, escaped the documented way.
		SessionLive: []string{`docker ps --format '{{"{{"}}.Names}}'`},
	}}
	if err := ValidateAgents(agents); err != nil {
		t.Fatalf("ValidateAgents rejected a valid config: %v", err)
	}
}

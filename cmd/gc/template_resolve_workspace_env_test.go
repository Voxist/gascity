package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveTemplateMergesWorkspaceEnv(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")

	params := &agentBuildParams{
		cityName: "city",
		cityPath: cityPath,
		workspace: &config.Workspace{
			Provider: "test",
			Env: map[string]string{
				"GC_TARGET_BRANCH": "boylec/develop",
				"FROM_WORKSPACE":   "ws",
			},
		},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{Name: "mayor"}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if got := tp.Env["GC_TARGET_BRANCH"]; got != "boylec/develop" {
		t.Errorf("GC_TARGET_BRANCH = %q, want %q", got, "boylec/develop")
	}
	if got := tp.Env["FROM_WORKSPACE"]; got != "ws" {
		t.Errorf("FROM_WORKSPACE = %q, want %q", got, "ws")
	}
}

func TestResolveTemplateAgentEnvWinsOverWorkspaceEnv(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")

	params := &agentBuildParams{
		cityName: "city",
		cityPath: cityPath,
		workspace: &config.Workspace{
			Provider: "test",
			Env: map[string]string{
				"GC_TARGET_BRANCH": "boylec/develop",
			},
		},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{
		Name: "mayor",
		Env:  map[string]string{"GC_TARGET_BRANCH": "boylec/special"},
	}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if got := tp.Env["GC_TARGET_BRANCH"]; got != "boylec/special" {
		t.Errorf("GC_TARGET_BRANCH = %q, want %q (agent env must override workspace env)", got, "boylec/special")
	}
}

func TestResolveTemplateDisablesProductMetricsForManagedAgent(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{Name: "worker", Env: map[string]string{
		execenv.UsageMetricsDisableEnv: "0",
		"BD_DISABLE_METRICS":           "leave-beads-alone",
	}}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if got := tp.Env[execenv.UsageMetricsDisableEnv]; got != execenv.UsageMetricsDisableValue {
		t.Fatalf("%s = %q, want %q", execenv.UsageMetricsDisableEnv, got, execenv.UsageMetricsDisableValue)
	}
	if got := tp.Env["BD_DISABLE_METRICS"]; got != "leave-beads-alone" {
		t.Fatalf("BD_DISABLE_METRICS = %q, want unchanged", got)
	}
}

// vp-w7cc: TOML-sourced env layers reference the CHILD session's identity
// (${GC_SESSION_NAME}, ${GC_AGENT}) — the identity agentEnv composes at
// Step 8. Expanding those layers against the controller process collapsed
// them: the supervisor carries neither var, so the OTEL_RESOURCE_ATTRIBUTES
// string the fleet configures materialized with every attribution attribute
// empty. Workspace, provider, and agent env must expand against the
// session-identity layer. The operator fingerprint (operatorEnv) deliberately
// stays controller-expanded — per-session identity must not fragment
// Launch-tier relaunch dedupe.
func TestResolveTemplateExpandsConfigEnvAgainstSessionIdentity(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")

	params := &agentBuildParams{
		cityName: "city",
		cityPath: cityPath,
		workspace: &config.Workspace{
			Provider: "test",
			Env: map[string]string{
				"WS_SESSION_PROBE": "session=${GC_SESSION_NAME},agent=${GC_AGENT}",
			},
		},
		providers: map[string]config.ProviderSpec{
			"test": {
				Command:    "echo",
				PromptMode: "none",
				Env: map[string]string{
					"OTEL_RESOURCE_ATTRIBUTES": "gc.bead_id=${GC_BEAD_ID},gc.session=${GC_SESSION_NAME},gc.agent=${GC_AGENT},gc.provider=claude",
				},
			},
		},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{Name: "mayor"}

	// The controller process carries a DIFFERENT session's identity; the
	// child's Step-8 identity must win, and the controller's must never leak.
	t.Setenv("GC_SESSION_NAME", "controller-session-must-not-leak")
	t.Setenv("GC_AGENT", "controller/agent-must-not-leak")
	// Hermetic against bead-bound test processes: a bead-less session expands
	// gc.bead_id empty by design, ambient trigger vars notwithstanding.
	t.Setenv("GC_BEAD_ID", "")

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if tp.SessionName == "" {
		t.Fatalf("tp.SessionName empty — fixture no longer composes the Step-8 identity")
	}
	want := "gc.bead_id=,gc.session=" + tp.SessionName + ",gc.agent=" + tp.SessionName + ",gc.provider=claude"
	if got := tp.Env["OTEL_RESOURCE_ATTRIBUTES"]; got != want {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, want %q (child-session identity, not the controller's)", got, want)
	}
	if got, want := tp.Env["WS_SESSION_PROBE"], "session="+tp.SessionName+",agent="+tp.SessionName; got != want {
		t.Errorf("WS_SESSION_PROBE = %q, want %q", got, want)
	}
}

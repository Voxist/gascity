package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// vp-w7cc: workspace and provider env authored in city.toml reference the
// CHILD session's identity (${GC_SESSION_NAME}, ${GC_AGENT}), which exists only
// in the env the session runtime will exec with, never in the controller
// process doing the expansion. The supervisor carries neither var; a CLI or
// agent spawning a session carries a DIFFERENT session's values, which is
// worse than absence. Every API session-env path must expand against the
// child's identity.

const (
	identityProbeWorkspaceKey = "WS_SESSION_PROBE"
	identityProbeProviderKey  = "OTEL_RESOURCE_ATTRIBUTES"
)

func setControllerIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("GC_SESSION_NAME", "controller-session-must-not-leak")
	t.Setenv("GC_AGENT", "controller/agent-must-not-leak")
}

func assertIdentityProbes(t *testing.T, label string, env map[string]string, sessionName, agent string) {
	t.Helper()
	for key, want := range map[string]string{
		identityProbeWorkspaceKey: "session=" + sessionName,
		identityProbeProviderKey:  "gc.session=" + sessionName + ",gc.agent=" + agent + ",gc.provider=claude",
	} {
		if got := env[key]; got != want {
			t.Errorf("%s[%s] = %q, want %q (child-session identity, not the controller's)", label, key, got, want)
		}
	}
}

func identityProbeFakeState(t *testing.T) *fakeState {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.cfg.Workspace.Env = map[string]string{
		identityProbeWorkspaceKey: "session=${GC_SESSION_NAME}",
	}
	fs.cfg.Agents[0].Provider = "identity-probe"
	fs.cfg.Providers["identity-probe"] = config.ProviderSpec{
		Command: "/bin/echo",
		Env: map[string]string{
			identityProbeProviderKey: "gc.session=${GC_SESSION_NAME},gc.agent=${GC_AGENT},gc.provider=claude",
		},
	}
	return fs
}

func TestResolveWorkerSessionRuntimeExpandsConfigEnvAgainstSessionIdentity(t *testing.T) {
	setControllerIdentity(t)
	srv := New(identityProbeFakeState(t))
	info := session.Info{
		ID:          "sess-1",
		Template:    "myrig/worker",
		Command:     "/bin/echo",
		WorkDir:     t.TempDir(),
		SessionName: "resumed-session-42",
		Alias:       "resumed-agent",
	}

	rt, err := srv.resolveWorkerSessionRuntime(info)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntime: %v", err)
	}
	if rt == nil {
		t.Fatal("resolveWorkerSessionRuntime() = nil")
	}
	assertIdentityProbes(t, "SessionEnv", rt.SessionEnv, "resumed-session-42", "resumed-agent")
	assertIdentityProbes(t, "Hints.Env", rt.Hints.Env, "resumed-session-42", "resumed-agent")
}

func TestBuildSessionResumeExpandsConfigEnvAgainstSessionIdentity(t *testing.T) {
	setControllerIdentity(t)
	srv := New(identityProbeFakeState(t))
	info := session.Info{
		ID:          "sess-1",
		Template:    "myrig/worker",
		Command:     "/bin/echo",
		WorkDir:     t.TempDir(),
		SessionName: "resumed-session-42",
		Alias:       "resumed-agent",
	}

	_, hints, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	assertIdentityProbes(t, "Hints.Env", hints.Env, "resumed-session-42", "resumed-agent")
}

// A create knows the new session's alias and, when one was given, its explicit
// name. A session name generated later from the bead ID is not known yet, so
// it expands EMPTY rather than to the controller's value.
func TestResolvedSessionConfigForProviderExpandsConfigEnvAgainstSessionIdentity(t *testing.T) {
	setControllerIdentity(t)
	workspaceEnv := map[string]string{identityProbeWorkspaceKey: "session=${GC_SESSION_NAME}"}
	resolved := &config.ResolvedProvider{
		Name:    "stub",
		Command: "/bin/echo",
		Env: map[string]string{
			identityProbeProviderKey: "gc.session=${GC_SESSION_NAME},gc.agent=${GC_AGENT},gc.provider=claude",
		},
	}
	for _, tc := range []struct {
		name, alias, explicitName string
		wantSession, wantAgent    string
	}{
		{name: "explicit name and alias", alias: "new-agent", explicitName: "new-session-7", wantSession: "new-session-7", wantAgent: "new-agent"},
		{name: "explicit name only", explicitName: "new-session-7", wantSession: "new-session-7", wantAgent: "new-session-7"},
		{name: "generated name", alias: "new-agent", wantSession: "", wantAgent: "new-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg, err := resolvedSessionConfigForProvider(
				cityPath,
				workspaceEnv,
				runtime.ProgramStatusOff,
				tc.alias,
				tc.explicitName,
				"myrig/worker",
				"Worker",
				"",
				nil,
				resolved,
				"",
				cityPath,
				nil,
			)
			if err != nil {
				t.Fatalf("resolvedSessionConfigForProvider: %v", err)
			}
			assertIdentityProbes(t, "SessionEnv", cfg.Runtime.SessionEnv, tc.wantSession, tc.wantAgent)
		})
	}
}

// Callers without a session identity keep expanding host context against the
// controller process, and an absent var still expands empty.
func TestCityAnchoredSessionEnvNilIdentityFallsBackToController(t *testing.T) {
	t.Setenv("VP_W7CC_HOST_CTX", "host-value")
	providerEnv := map[string]string{"VP_W7CC_PROBE": "ctx:${VP_W7CC_HOST_CTX}"}
	env := cityAnchoredSessionEnv(t.TempDir(), nil, providerEnv, nil)
	if got, want := env["VP_W7CC_PROBE"], "ctx:host-value"; got != want {
		t.Errorf("VP_W7CC_PROBE = %q, want %q", got, want)
	}
}

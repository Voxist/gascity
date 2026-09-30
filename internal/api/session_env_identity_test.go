package api

import "testing"

// vp-w7cc: provider env authored in city.toml references the CHILD session's
// identity (${GC_SESSION_NAME}, ${GC_AGENT}), which exists only in the env the
// session runtime will exec with — never in the controller process doing the
// expansion (the supervisor carries neither var; a spawning agent carries a
// DIFFERENT session's values, which is worse than absence). Expansion must
// prefer the session identity map and fall back to the controller process only
// for host context.
func TestCityAnchoredSessionEnvExpandsProviderEnvAgainstSessionIdentity(t *testing.T) {
	t.Setenv("GC_SESSION_NAME", "controller-session-must-not-leak")
	t.Setenv("GC_AGENT", "controller/agent-must-not-leak")
	providerEnv := map[string]string{
		"OTEL_RESOURCE_ATTRIBUTES": "gc.bead_id=${GC_BEAD_ID},gc.session=${GC_SESSION_NAME},gc.agent=${GC_AGENT},gc.provider=claude",
	}
	identity := map[string]string{
		"GC_SESSION_NAME": "voxist.executor-9",
		"GC_AGENT":        "voxist-platform/voxist.executor-9",
	}
	env := cityAnchoredSessionEnv(t.TempDir(), nil, providerEnv, identity)
	want := "gc.bead_id=,gc.session=voxist.executor-9,gc.agent=voxist-platform/voxist.executor-9,gc.provider=claude"
	if got := env["OTEL_RESOURCE_ATTRIBUTES"]; got != want {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, want %q", got, want)
	}
}

// Callers without session identity (create paths, before the session exists)
// keep today's behavior: expansion falls back to the controller process for
// host context, and an absent var still expands empty rather than erroring.
func TestCityAnchoredSessionEnvNilIdentityFallsBackToController(t *testing.T) {
	t.Setenv("VP_W7CC_HOST_CTX", "host-value")
	providerEnv := map[string]string{"VP_W7CC_PROBE": "ctx:${VP_W7CC_HOST_CTX}"}
	env := cityAnchoredSessionEnv(t.TempDir(), nil, providerEnv, nil)
	if got, want := env["VP_W7CC_PROBE"], "ctx:host-value"; got != want {
		t.Errorf("VP_W7CC_PROBE = %q, want %q", got, want)
	}
}

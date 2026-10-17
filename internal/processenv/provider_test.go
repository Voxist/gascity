package processenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsProviderCredentialEnvMatchesCuratedAllowlist(t *testing.T) {
	for _, prefix := range providerCredentialEnvPrefixes {
		key := prefix + "TEST_VALUE"
		if !IsProviderCredentialEnv(key) {
			t.Errorf("IsProviderCredentialEnv(%q) = false, want true for prefix %q", key, prefix)
		}
	}
	for key := range providerCredentialEnvKeys {
		if !IsProviderCredentialEnv(key) {
			t.Errorf("IsProviderCredentialEnv(%q) = false, want true for exact key", key)
		}
	}
}

func TestIsProviderCredentialEnvRejectsNearMisses(t *testing.T) {
	for _, key := range []string{
		"",
		"ANTHROPIC",
		"OPENROUTER",
		"AWS_ACCESS_KEY_ID_EXTRA",
		"AWS_EXECUTION_ENV",
		"AWS_PAGER",
		"AWS_VAULT",
		"GC_RIG",
		"GC_SESSION_NAME",
		"CUSTOM_PROVIDER_TOKEN",
	} {
		if IsProviderCredentialEnv(key) {
			t.Errorf("IsProviderCredentialEnv(%q) = true, want false", key)
		}
	}
}

func TestProviderProcessPassthroughEnvIncludesProviderAndRuntimeBaseline(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("LANG", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "test-anthropic-token")
	t.Setenv("OLLAMA_API_KEY", "test-ollama-token")
	t.Setenv("XIAOMI_API_KEY", "test-xiaomi-key")
	t.Setenv("AWS_ACCESS_KEY_ID", "test-aws-key")
	t.Setenv("AWS_PAGER", "less")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "nested")
	t.Setenv("CODEX_THREAD_ID", "thread-123")
	t.Setenv("CODEX_CI", "true")

	got := ProviderProcessPassthroughEnv()

	for key, want := range map[string]string{
		"HOME":                   homeDir,
		"PATH":                   "/usr/local/bin:/usr/bin:/bin",
		"LANG":                   "en_US.UTF-8",
		"LC_ALL":                 "",
		"LC_CTYPE":               "",
		"XDG_CONFIG_HOME":        filepath.Join(homeDir, ".config"),
		"XDG_STATE_HOME":         filepath.Join(homeDir, ".local", "state"),
		"ANTHROPIC_AUTH_TOKEN":   "test-anthropic-token",
		"OLLAMA_API_KEY":         "test-ollama-token",
		"XIAOMI_API_KEY":         "test-xiaomi-key",
		"AWS_ACCESS_KEY_ID":      "test-aws-key",
		"CLAUDECODE":             "",
		"CLAUDE_CODE_ENTRYPOINT": "",
		"CODEX_THREAD_ID":        "",
		"CODEX_CI":               "",
	} {
		if got[key] != want {
			t.Errorf("ProviderProcessPassthroughEnv()[%s] = %q, want %q", key, got[key], want)
		}
	}
	if _, ok := got["AWS_PAGER"]; ok {
		t.Errorf("ProviderProcessPassthroughEnv()[AWS_PAGER] = %q, want absent", got["AWS_PAGER"])
	}
}

func TestProviderProcessPassthroughEnvKeepsExplicitLocaleAndXDG(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("LC_CTYPE", "UTF-8")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/custom-config")
	t.Setenv("XDG_STATE_HOME", "/tmp/custom-state")

	got := ProviderProcessPassthroughEnv()

	for key, want := range map[string]string{
		"LANG":            "C.UTF-8",
		"LC_ALL":          "en_US.UTF-8",
		"LC_CTYPE":        "UTF-8",
		"XDG_CONFIG_HOME": "/tmp/custom-config",
		"XDG_STATE_HOME":  "/tmp/custom-state",
	} {
		if got[key] != want {
			t.Errorf("ProviderProcessPassthroughEnv()[%s] = %q, want %q", key, got[key], want)
		}
	}
}

// The controller token is controller scope. Every session-env builder starts
// from this map, and the map is an OVERLAY on an environment the child already
// inherits — the tmux server's global env, or os.Environ() on the
// subprocess/ACP paths — so an omitted key is an inherited key.
// Present-and-empty is the only value that withholds it.
func TestProviderProcessPassthroughEnvPinsControllerOnlyKeysEmpty(t *testing.T) {
	for _, key := range ControllerOnlyEnvKeys {
		t.Setenv(key, "controller-scope-value")
	}

	got := ProviderProcessPassthroughEnv()

	for _, key := range ControllerOnlyEnvKeys {
		val, ok := got[key]
		if !ok {
			t.Errorf("ProviderProcessPassthroughEnv() omits %s; want present and empty so it overrides the inherited value", key)
			continue
		}
		if val != "" {
			t.Errorf("ProviderProcessPassthroughEnv()[%s] = %q, want empty", key, val)
		}
	}
}

// Pinning the keys is not enough on its own: config-authored values are expanded
// against the controller process, so "$GC_CONTROLLER_TOKEN" would copy the token
// into a session variable no key-level guard is watching.
func TestExpandSessionEnvValueMasksControllerOnlyKeys(t *testing.T) {
	t.Setenv("GC_CONTROLLER_TOKEN", "super-secret-controller-token")
	t.Setenv("GC_CONTROLLER_TRACE", "on")

	for _, tc := range []struct{ in, want string }{
		{"$GC_CONTROLLER_TOKEN", ""},
		{"${GC_CONTROLLER_TOKEN}", ""},
		{"Bearer $GC_CONTROLLER_TOKEN", "Bearer "},
		{"$GC_CONTROLLER_TRACE", "on"},
		{"trace=${GC_CONTROLLER_TRACE}", "trace=on"},
	} {
		if got := ExpandSessionEnvValue(tc.in); got != tc.want {
			t.Errorf("ExpandSessionEnvValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// vp-w7cc: provider/workspace env is authored against the CHILD session's
// identity (${GC_AGENT}, ${GC_SESSION_NAME}), which exists only once the
// session runtime composes it. Expanding those references against the
// controller process silently collapsed every attribution attribute
// fleet-wide. The identity-aware expansion consults the child's identity map
// FIRST, then falls back to the controller process for everything else.
func TestExpandSessionEnvValueWithIdentityPrefersChildIdentity(t *testing.T) {
	// Pin hermetic against bead-bound test processes: the fallback path reads
	// the controller env, so an ambient GC_BEAD_ID must not leak into the
	// expectation (a bead-less identity map entry means EMPTY, always).
	t.Setenv("GC_BEAD_ID", "")
	identity := map[string]string{
		"GC_AGENT":        "voxist-platform/voxist.executor-1",
		"GC_SESSION_NAME": "voxist.executor-9",
	}
	for _, tc := range []struct{ in, want string }{
		{
			in:   "gc.bead_id=${GC_BEAD_ID},gc.session=${GC_SESSION_NAME},gc.agent=${GC_AGENT},gc.provider=claude",
			want: "gc.bead_id=,gc.session=voxist.executor-9,gc.agent=voxist-platform/voxist.executor-1,gc.provider=claude",
		},
	} {
		if got := ExpandSessionEnvValueWithIdentity(tc.in, identity); got != tc.want {
			t.Errorf("ExpandSessionEnvValueWithIdentity(%q, identity) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExpandSessionEnvValueWithIdentityFallsBackToController(t *testing.T) {
	t.Setenv("VP_W7CC_CONTROLLER_VAR", "from-controller")
	identity := map[string]string{"GC_AGENT": "child-agent"}
	in := "${VP_W7CC_CONTROLLER_VAR}/${GC_AGENT}"
	if got := ExpandSessionEnvValueWithIdentity(in, identity); got != "from-controller/child-agent" {
		t.Errorf("ExpandSessionEnvValueWithIdentity(%q, identity) = %q, want %q", in, got, "from-controller/child-agent")
	}
}

// The identity map must never become a laundering path for controller-only
// credentials: the mask wins even when the map carries the key.
func TestExpandSessionEnvValueWithIdentityStillMasksControllerOnlyKeys(t *testing.T) {
	t.Setenv("GC_CONTROLLER_TOKEN", "super-secret-controller-token")
	identity := map[string]string{"GC_CONTROLLER_TOKEN": "leaked-anyway"}
	for _, tc := range []struct{ in, want string }{
		{"${GC_CONTROLLER_TOKEN}", ""},
		{"Bearer ${GC_CONTROLLER_TOKEN}", "Bearer "},
	} {
		if got := ExpandSessionEnvValueWithIdentity(tc.in, identity); got != tc.want {
			t.Errorf("ExpandSessionEnvValueWithIdentity(%q, identity) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TZ passes through to spawned provider sessions so in-session time
// reasoning (e.g. `gc order check`) agrees with the supervisor's wall clock
// instead of defaulting to UTC in the constructed env.
func TestProviderProcessPassthroughEnvIncludesTZ(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	m := ProviderProcessPassthroughEnv()
	if m["TZ"] != "America/New_York" {
		t.Errorf(`m["TZ"] = %q, want "America/New_York"`, m["TZ"])
	}
}

func TestProviderProcessPassthroughEnvOmitsUnsetTZ(t *testing.T) {
	t.Setenv("TZ", "")
	m := ProviderProcessPassthroughEnv()
	if v, ok := m["TZ"]; ok {
		t.Errorf(`m["TZ"] = %q present, want absent when host TZ is unset`, v)
	}
}

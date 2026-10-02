package runtime

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	workerbuiltin "github.com/gastownhall/gascity/internal/worker/builtin"
)

// tierStandardConfig is a Claude-harness session whose provider block carries
// a serving identity and the credentials for it, in both the merged Env and
// the operator-authored OperatorEnv — the shape a [providers.tier-standard]
// block with ANTHROPIC_* env produces.
func tierStandardConfig() Config {
	env := map[string]string{
		"GC_CITY":                        "city",
		"GC_TEMPLATE":                    "executor",
		"ANTHROPIC_BASE_URL":             "https://api.z.ai/api/anthropic",
		"ANTHROPIC_MODEL":                "glm-5.3-flash",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "glm-5.3-flash",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "glm-5.3-flash",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "glm-5.3-flash",
		"ANTHROPIC_SMALL_FAST_MODEL":     "glm-5.3-flash",
		"ANTHROPIC_AUTH_TOKEN":           "token-one",
		"ANTHROPIC_API_KEY":              "key-one",
		"CLAUDE_CODE_OAUTH_TOKEN":        "oauth-one",
		"OTEL_LOGS_EXPORTER":             "otlp",
		"PATH":                           "/usr/bin",
	}
	operator := map[string]string{}
	for _, k := range []string{
		"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY",
		"OTEL_LOGS_EXPORTER",
	} {
		operator[k] = env[k]
	}
	return Config{Command: "claude", Env: env, OperatorEnv: operator}
}

// setEnv returns cfg with key=val written to Env, and to OperatorEnv when
// operator is true. It clones both maps so the base fixture is never shared.
func setEnv(cfg Config, key, val string, operator bool) Config {
	cfg.Env = envWith(cfg.Env, key, val)
	if operator {
		cfg.OperatorEnv = envWith(cfg.OperatorEnv, key, val)
	}
	return cfg
}

type fingerprintMoves struct {
	core, launch, provision, live, config bool
}

func movesBetween(a, b Config) fingerprintMoves {
	return fingerprintMoves{
		core:      CoreFingerprint(a) != CoreFingerprint(b),
		launch:    LaunchFingerprint(a) != LaunchFingerprint(b),
		provision: ProvisionFingerprint(a) != ProvisionFingerprint(b),
		live:      LiveFingerprint(a) != LiveFingerprint(b),
		config:    ConfigFingerprint(a) != ConfigFingerprint(b),
	}
}

var (
	launchDrift    = fingerprintMoves{core: true, launch: true, config: true}
	provisionDrift = fingerprintMoves{core: true, provision: true, config: true}
	noDrift        = fingerprintMoves{}
)

// TestServingIdentityFingerprint pins the v7 rule: a provider's serving
// identity (base URL, model selection) is LAUNCH identity — a same-name
// provider whose content flips vendor relaunches its sessions — while its
// credentials are never hashed, and unrelated env behaves exactly as before.
func TestServingIdentityFingerprint(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(Config) Config
		want   fingerprintMoves
	}{
		// The incident: an external failover rewrites [providers.tier-standard]
		// in place, z.ai <-> Anthropic.
		{"vendor flip rewrites base URL and every model key", func(c Config) Config {
			c = setEnv(c, "ANTHROPIC_BASE_URL", "https://api.anthropic.com", true)
			for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL"} {
				c = setEnv(c, k, "claude-sonnet-5", true)
			}
			return c
		}, launchDrift},
		{"base URL only", func(c Config) Config {
			return setEnv(c, "ANTHROPIC_BASE_URL", "https://api.anthropic.com", true)
		}, launchDrift},
		{"model only", func(c Config) Config { return setEnv(c, "ANTHROPIC_MODEL", "claude-sonnet-5", true) }, launchDrift},
		// The serving env of an [upstreams.*] block (or the process passthrough)
		// lands in Env but never in OperatorEnv; the hash must still see it.
		{"base URL changed in resolved Env only (upstream render)", func(c Config) Config {
			return setEnv(c, "ANTHROPIC_BASE_URL", "https://gateway.example.com", false)
		}, launchDrift},
		{"non-Claude harness base URL (upstream_env binding)", func(c Config) Config {
			return setEnv(c, "OPENAI_BASE_URL", "https://gateway.example.com/v1", false)
		}, launchDrift},
		{"serving key removed", func(c Config) Config {
			c.Env = envWith(c.Env, "GC_CITY", c.Env["GC_CITY"]) // clone before delete
			delete(c.Env, "ANTHROPIC_SMALL_FAST_MODEL")
			return c
		}, launchDrift},

		// Credentials: a rotation moves nothing, wherever the key is written.
		{"auth token rotation", func(c Config) Config { return setEnv(c, "ANTHROPIC_AUTH_TOKEN", "token-two", true) }, noDrift},
		{"api key rotation", func(c Config) Config { return setEnv(c, "ANTHROPIC_API_KEY", "key-two", true) }, noDrift},
		{"oauth token rotation in passthrough", func(c Config) Config {
			return setEnv(c, "CLAUDE_CODE_OAUTH_TOKEN", "oauth-two", false)
		}, noDrift},
		{"secret-named operator key rotation", func(c Config) Config {
			return setEnv(c, "VENDOR_CLIENT_SECRET", "secret-two", true)
		}, noDrift},
		{"every credential rotated at once", func(c Config) Config {
			c = setEnv(c, "ANTHROPIC_AUTH_TOKEN", "token-two", true)
			c = setEnv(c, "ANTHROPIC_API_KEY", "key-two", true)
			return setEnv(c, "CLAUDE_CODE_OAUTH_TOKEN", "oauth-two", false)
		}, noDrift},

		// Unrelated env: unchanged behavior.
		{"non-allow-listed passthrough env", func(c Config) Config { return setEnv(c, "PATH", "/opt/bin", false) }, noDrift},
		{"operator-authored non-credential env", func(c Config) Config {
			return setEnv(c, "OTEL_LOGS_EXPORTER", "none", true)
		}, launchDrift},
		{"allow-listed box env", func(c Config) Config { return setEnv(c, "GC_CITY", "other-city", false) }, provisionDrift},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := tierStandardConfig()
			mutated := tc.mutate(tierStandardConfig())
			if got := movesBetween(base, mutated); got != tc.want {
				t.Errorf("fingerprint moves = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestServingIdentityFingerprintAbsentKeysContributeNothing keeps every config
// that sets no serving key byte-identical between "no serving section" and the
// pre-v7 framing (nothing written), so nil/empty Env still agree.
func TestServingIdentityFingerprintAbsentKeysContributeNothing(t *testing.T) {
	plain := Config{Command: "agent", Env: map[string]string{"GC_CITY": "c"}}
	withCredentialOnly := setEnv(plain, "ANTHROPIC_API_KEY", "k", false)
	if got := movesBetween(plain, withCredentialOnly); got != noDrift {
		t.Errorf("credential-only env moved %+v, want no drift", got)
	}
}

// TestServingIdentityEnvKeysPin freezes the hashed serving set. Adding a key
// moves the fingerprint of every config that sets it (bump FingerprintVersion);
// a credential here would make every rotation relaunch the fleet.
func TestServingIdentityEnvKeysPin(t *testing.T) {
	want := []string{
		"AMP_URL",
		"ANTHROPIC_BASE_URL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
		"COPILOT_PROVIDER_BASE_URL", "GOOGLE_GEMINI_BASE_URL", "KIMI_BASE_URL",
		"OPENAI_BASE_URL", "ZCODE_BASE_URL",
	}
	var got []string
	for k := range servingIdentityEnvKeys {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("servingIdentityEnvKeys drift:\n got  %v\n want %v", got, want)
	}
	for k := range servingIdentityEnvKeys {
		if isCredentialEnvKey(k) {
			t.Errorf("serving identity key %q is credential-shaped; credentials must never be hashed", k)
		}
		if envFingerprintAllow[k] {
			t.Errorf("serving identity key %q is also allow-listed; it would land in the PROVISION half too", k)
		}
	}
}

// TestServingIdentityCoversBuiltinUpstreamBindings ties the base-URL keys to
// the builtin harness profiles: every upstream_env.base_url binding is hashed
// serving identity, and every api_key/auth_token binding is credential-shaped
// (never hashed). A new builtin harness fails here until it is classified.
func TestServingIdentityCoversBuiltinUpstreamBindings(t *testing.T) {
	for name, spec := range workerbuiltin.BuiltinProviders() {
		if k := spec.UpstreamBaseURLEnv; k != "" && !servingIdentityEnvKeys[k] {
			t.Errorf("builtin %q upstream base URL env %q is not in servingIdentityEnvKeys", name, k)
		}
		for _, k := range []string{spec.UpstreamAPIKeyEnv, spec.UpstreamAuthTokenEnv} {
			if k != "" && !isCredentialEnvKey(k) {
				t.Errorf("builtin %q upstream credential env %q is not credential-shaped; it would be hashed via OperatorEnv", name, k)
			}
		}
	}
}

func TestIsCredentialEnvKey(t *testing.T) {
	cases := map[string]bool{
		"ANTHROPIC_AUTH_TOKEN":     true,
		"ANTHROPIC_API_KEY":        true,
		"CLAUDE_CODE_OAUTH_TOKEN":  true,
		"GC_CONTROLLER_TOKEN":      true,
		"AWS_SECRET_ACCESS_KEY":    true,
		"client_secret":            true,
		"ANTHROPIC_AUTH_TOKEN_ZAI": true,
		"OPENAI_API_KEY_BACKUP":    true,
		"ANTHROPIC_BASE_URL":       false,
		"ANTHROPIC_MODEL":          false,
		"MAX_THINKING_TOKENS":      false,
		"OTEL_LOGS_EXPORTER":       false,
	}
	for key, want := range cases {
		if got := isCredentialEnvKey(key); got != want {
			t.Errorf("isCredentialEnvKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// TestServingIdentityDriftIsDiagnosed: the drift breakdown names the serving
// identity, and the drift log prints the new vendor without ever printing a
// credential.
func TestServingIdentityDriftIsDiagnosed(t *testing.T) {
	stored := tierStandardConfig()
	current := setEnv(tierStandardConfig(), "ANTHROPIC_BASE_URL", "https://api.anthropic.com", true)
	current = setEnv(current, "ANTHROPIC_AUTH_TOKEN", "token-two", true)

	if got := CoreFingerprintDriftFields(CoreFingerprintBreakdown(stored), current); !containsString(got, "ServingIdentity") {
		t.Errorf("drifted fields = %v, want ServingIdentity among them", got)
	}
	raw, err := json.Marshal(CoreFingerprintBreakdown(stored))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	LogCoreFingerprintDrift(&buf, "executor", string(raw), current)
	out := buf.String()
	if !strings.Contains(out, "ServingIdentity") || !strings.Contains(out, "https://api.anthropic.com") {
		t.Errorf("drift log does not name the serving identity change:\n%s", out)
	}
	for _, secret := range []string{"token-one", "token-two", "key-one", "oauth-one"} {
		if strings.Contains(out, secret) {
			t.Errorf("drift log leaks credential value %q:\n%s", secret, out)
		}
	}
}

// TestServingIdentityVersionBumpRebaselines: hashes written by the previous
// binary carry v6 and are classified for silent rebaseline (no drain), while a
// same-version v7 mismatch stays real drift.
func TestServingIdentityVersionBumpRebaselines(t *testing.T) {
	cfg := tierStandardConfig()
	for _, fp := range []string{CoreFingerprint(cfg), LaunchFingerprint(cfg), ProvisionFingerprint(cfg)} {
		if !strings.HasPrefix(fp, "v7:") {
			t.Fatalf("fingerprint %q does not carry the v7 prefix", fp)
		}
		previous := "v6:" + strings.TrimPrefix(fp, "v7:")
		if !IsLegacyOrMismatchedVersion(previous) || !IsVersionMismatchedHash(previous) {
			t.Errorf("v6 hash %q is not classified for silent rebaseline", previous)
		}
		if IsLegacyOrMismatchedVersion(fp) {
			t.Errorf("current hash %q is classified for rebaseline; real drift would be swallowed", fp)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

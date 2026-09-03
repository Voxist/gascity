package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/processenv"
)

// credentialBinding is one declared credential role of a provider, resolved
// down to the environment variable that actually holds the secret.
//
// Exactly one of SourceVar and Refusal is set. A refused role is reported, not
// skipped: on a credential path a silently-omitted entry is the failure that
// matters, because the operator concludes there was nothing to rotate.
type credentialBinding struct {
	// Role is the upstream_env role this entry came from: "api_key" or
	// "auth_token".
	Role string
	// EnvKey is the harness env var the role binds to, e.g. ANTHROPIC_API_KEY.
	EnvKey string
	// SourceVar is the environment variable that actually holds the
	// credential — the only thing a rotation may write. Empty when Refusal
	// is set.
	SourceVar string
	// Inherited reports that the provider's env declares nothing for EnvKey,
	// so the harness receives it straight from the supervisor environment
	// under its own name. SourceVar then equals EnvKey.
	Inherited bool
	// Refusal explains why this role cannot be rotated. Empty when SourceVar
	// is set.
	Refusal string
}

// providerCredentialSources resolves which environment variables back a
// provider's credentials, using the provider's own declaration rather than any
// inference from variable names.
//
// A provider's [config.UpstreamEnvBinding] already states the roles
// structurally: api_key and auth_token name the harness env vars that carry a
// secret, base_url names the one that carries an endpoint. Rotation follows
// only the first two. base_url is absent here and must stay absent: assigning
// a credential to the variable behind a base URL destroys the provider's
// routing, which is what made the previous implementation unsafe to run.
//
// Deliberately NOT used: the envArgvSafe allow-list in internal/runtime. It
// answers "may this value appear in argv?", whose fail-safe is "unknown means
// assume secret" because the cost of being wrong is a temp file. This asks
// "may this variable be overwritten?", whose cost of being wrong is destroying
// a live value — the opposite fail-safe. ANTHROPIC_BASE_URL is not on that
// allow-list, so reusing it here would classify the endpoint as a credential
// and overwrite it.
//
// A provider declaring neither credential role yields no entries; the caller
// refuses rather than guessing which of its env keys holds a secret.
func providerCredentialSources(resolved *config.ResolvedProvider) []credentialBinding {
	if resolved == nil {
		return nil
	}
	roles := []struct{ role, envKey string }{
		{"api_key", resolved.UpstreamEnv.APIKey},
		{"auth_token", resolved.UpstreamEnv.AuthToken},
	}

	out := make([]credentialBinding, 0, len(roles))
	for _, r := range roles {
		if r.envKey == "" {
			continue
		}
		out = append(out, resolveCredentialBinding(resolved.Name, r.role, r.envKey, resolved.Env))
	}
	return out
}

// resolveCredentialBinding resolves one declared role against the provider's
// merged env map.
func resolveCredentialBinding(providerName, role, envKey string, env map[string]string) credentialBinding {
	b := credentialBinding{Role: role, EnvKey: envKey}
	value, ok := env[envKey]
	switch {
	case !ok:
		// The provider declares nothing for this key, so the harness reads it
		// from the session env. processenv.ProviderProcessPassthroughEnv
		// forwards every variable on the provider-credential allowlist from
		// the supervisor into that env under its own name, so an allowlisted
		// key does reach the agent and IS rotatable — as itself. A key off
		// that allowlist reaches nothing, and there is no variable to rotate.
		if processenv.IsProviderCredentialEnv(envKey) {
			b.SourceVar = envKey
			b.Inherited = true
			break
		}
		b.Refusal = fmt.Sprintf("provider %q binds upstream_env.%s to %s, but its env sets no such key and %s is not forwarded from the supervisor environment, so nothing supplies it",
			providerName, role, envKey, envKey)
	case value == "":
		b.Refusal = fmt.Sprintf("%s is set empty, which withholds the variable rather than supplying a credential", envKey)
	default:
		if source, isRef := processenv.SoleEnvRef(value); isRef {
			b.SourceVar = source
			break
		}
		if !processenv.HasEnvRef(value) {
			b.Refusal = fmt.Sprintf("%s is a literal value, so the credential is inlined in config; change it where it is written, not in the environment", envKey)
			break
		}
		b.Refusal = fmt.Sprintf("%s interpolates a variable but also carries literal text or a second reference, so no single variable holds the credential on its own", envKey)
	}
	return b
}

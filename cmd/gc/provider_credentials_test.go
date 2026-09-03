package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestProviderCredentialSourcesRotatesOnlyCredentialRoles is the direct guard
// on the defect that made `gc provider rotate-key` unsafe (ga-i86nb): the
// shipped code collected every ${VAR} referenced anywhere in the provider env
// and assigned the API key to all of them, so a base URL written as
// "${ZAI_BASE_URL}" was overwritten with the credential and provider routing
// broke fleet-wide.
//
// The provider spec here is the shape our own city runs — a zai harness whose
// base URL and credential are BOTH env refs. That is the shape that triggers
// the bug; a static-literal base URL is the one shape that cannot.
func TestProviderCredentialSourcesRotatesOnlyCredentialRoles(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name: "zai",
		UpstreamEnv: config.UpstreamEnvBinding{
			BaseURL:   "ANTHROPIC_BASE_URL",
			APIKey:    "ANTHROPIC_API_KEY",
			AuthToken: "ANTHROPIC_AUTH_TOKEN",
		},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":   "${ZAI_BASE_URL}",
			"ANTHROPIC_API_KEY":    "${ANTHROPIC_AUTH_TOKEN_ZAI}",
			"ANTHROPIC_AUTH_TOKEN": "${ANTHROPIC_AUTH_TOKEN_ZAI}",
		},
	}

	got := providerCredentialSources(resolved)

	sources := map[string]bool{}
	for _, b := range got {
		if b.Refusal == "" {
			sources[b.SourceVar] = true
		}
	}

	if !sources["ANTHROPIC_AUTH_TOKEN_ZAI"] {
		t.Errorf("credential source ANTHROPIC_AUTH_TOKEN_ZAI not resolved; got %+v", got)
	}
	// The whole point. The base URL's source var is not a credential and must
	// never be offered for rotation.
	if sources["ZAI_BASE_URL"] {
		t.Errorf("ZAI_BASE_URL was resolved as a credential source; it backs upstream_env.base_url — writing the API key to it destroys provider routing (got %+v)", got)
	}
	for _, b := range got {
		if b.Role == "base_url" {
			t.Errorf("base_url appeared as a credential role: %+v", b)
		}
		if b.EnvKey == "ANTHROPIC_BASE_URL" {
			t.Errorf("the base URL env key appeared as a rotation target: %+v", b)
		}
	}
}

func TestProviderCredentialSourcesRefusals(t *testing.T) {
	tests := []struct {
		name        string
		resolved    *config.ResolvedProvider
		wantRefusal string // substring the refusal must name
		wantRole    string
	}{
		{
			name: "static literal credential is refused, not rewritten",
			resolved: &config.ResolvedProvider{
				Name:        "inline",
				UpstreamEnv: config.UpstreamEnvBinding{APIKey: "ANTHROPIC_API_KEY"},
				Env:         map[string]string{"ANTHROPIC_API_KEY": "sk-ant-inlined"},
			},
			wantRefusal: "literal",
			wantRole:    "api_key",
		},
		{
			// The shipped code assigned the key VERBATIM to any value merely
			// containing "${", so "Bearer ${K}" became the bare key.
			name: "reference wrapped in literal text is refused",
			resolved: &config.ResolvedProvider{
				Name:        "hdr",
				UpstreamEnv: config.UpstreamEnvBinding{AuthToken: "ANTHROPIC_AUTH_TOKEN"},
				Env:         map[string]string{"ANTHROPIC_AUTH_TOKEN": "Bearer ${ACME_KEY}"},
			},
			wantRefusal: "literal text",
			wantRole:    "auth_token",
		},
		{
			// An undeclared key that the supervisor does NOT forward reaches
			// the harness from nowhere, so there is no variable to rotate.
			name: "undeclared, non-forwarded binding is refused",
			resolved: &config.ResolvedProvider{
				Name:        "unset",
				UpstreamEnv: config.UpstreamEnvBinding{APIKey: "MY_HARNESS_TOKEN"},
				Env:         map[string]string{"ANTHROPIC_BASE_URL": "https://example.test"},
			},
			wantRefusal: "sets no such key",
			wantRole:    "api_key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := providerCredentialSources(tt.resolved)
			if len(got) != 1 {
				t.Fatalf("providerCredentialSources = %+v; want exactly one entry", got)
			}
			if got[0].Role != tt.wantRole {
				t.Errorf("role = %q; want %q", got[0].Role, tt.wantRole)
			}
			if got[0].Refusal == "" {
				t.Fatalf("entry was accepted with source %q; want a refusal", got[0].SourceVar)
			}
			if !strings.Contains(got[0].Refusal, tt.wantRefusal) {
				t.Errorf("refusal = %q; want it to name %q", got[0].Refusal, tt.wantRefusal)
			}
			if got[0].SourceVar != "" {
				t.Errorf("refused entry still carries SourceVar %q; a refusal must offer no rotation target", got[0].SourceVar)
			}
		})
	}
}

// TestProviderCredentialSourcesHonoursBareDollarRef pins the grammar gap that
// made the shipped command exit 0 having rotated nothing: its regex matched
// only ${VAR}, while session start expands with os.Expand, which also honors
// bare $VAR — the form expandEnvMap's own doc comment uses as its example
// (cmd/gc/cmd_start.go:1552).
func TestProviderCredentialSourcesHonoursBareDollarRef(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:        "bare",
		UpstreamEnv: config.UpstreamEnvBinding{APIKey: "ANTHROPIC_API_KEY"},
		Env:         map[string]string{"ANTHROPIC_API_KEY": "$ACME_KEY"},
	}
	got := providerCredentialSources(resolved)
	if len(got) != 1 {
		t.Fatalf("providerCredentialSources = %+v; want one entry", got)
	}
	if got[0].Refusal != "" {
		t.Fatalf("bare $VAR refused (%q); session start expands it, so it is rotatable", got[0].Refusal)
	}
	if got[0].SourceVar != "ACME_KEY" {
		t.Errorf("SourceVar = %q; want ACME_KEY", got[0].SourceVar)
	}
}

// TestProviderCredentialSourcesInheritsForwardedKey covers the commonest
// shape: a provider that declares no env entry for its credential at all. The
// harness still receives the variable, because
// processenv.ProviderProcessPassthroughEnv forwards every provider-credential
// key from the supervisor environment under its own name
// (internal/processenv/provider.go:219-226). So the rotation target is that
// key itself, and reporting "nothing to rotate" here would be wrong.
func TestProviderCredentialSourcesInheritsForwardedKey(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:        "claude",
		UpstreamEnv: config.UpstreamEnvBinding{APIKey: "ANTHROPIC_API_KEY"},
		Env:         map[string]string{},
	}
	got := providerCredentialSources(resolved)
	if len(got) != 1 {
		t.Fatalf("providerCredentialSources = %+v; want one entry", got)
	}
	if got[0].Refusal != "" {
		t.Fatalf("refused (%q); ANTHROPIC_API_KEY is forwarded from the supervisor env, so it is rotatable", got[0].Refusal)
	}
	if got[0].SourceVar != "ANTHROPIC_API_KEY" {
		t.Errorf("SourceVar = %q; want ANTHROPIC_API_KEY", got[0].SourceVar)
	}
	if !got[0].Inherited {
		t.Error("Inherited = false; the operator needs to see that this comes from the ambient environment, not from provider config")
	}
}

// TestProviderCredentialSourcesNoBindingDeclared covers the case the command
// must refuse outright rather than guess at: a provider that declares no
// credential role at all. Guessing here is what the envArgvSafe allow-list
// would do, and it errs toward "assume credential" — the wrong direction when
// the cost of a false positive is overwriting a live value.
func TestProviderCredentialSourcesNoBindingDeclared(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:        "nobinding",
		UpstreamEnv: config.UpstreamEnvBinding{BaseURL: "ANTHROPIC_BASE_URL"},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL": "${ACME_BASE_URL}",
			"ANTHROPIC_API_KEY":  "${ACME_KEY}",
		},
	}
	if got := providerCredentialSources(resolved); len(got) != 0 {
		t.Fatalf("providerCredentialSources = %+v; want none — the provider declares no api_key or auth_token binding, and ACME_KEY must not be inferred from the key's name", got)
	}
}

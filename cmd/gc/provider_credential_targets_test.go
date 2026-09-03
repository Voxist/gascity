package main

import (
	"strings"
	"testing"
)

// TestCredentialWriteTargetsRefusesDivergentRoles guards the branch that
// prevents a --set from overwriting a credential the operator did not name.
// When api_key and auth_token resolve to DIFFERENT variables they hold
// separate secrets, and writing one supplied value to both would silently
// destroy the other.
func TestCredentialWriteTargetsRefusesDivergentRoles(t *testing.T) {
	bindings := []credentialBinding{
		{Role: "api_key", EnvKey: "ANTHROPIC_API_KEY", SourceVar: "ACME_KEY"},
		{Role: "auth_token", EnvKey: "ANTHROPIC_AUTH_TOKEN", SourceVar: "ACME_TOKEN"},
	}

	targets, err := credentialWriteTargets(bindings, "")
	if err == nil {
		t.Fatalf("credentialWriteTargets = %v; want a refusal — the two roles hold separate credentials", targets)
	}
	for _, want := range []string{"ACME_KEY", "ACME_TOKEN", "--role"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// Naming one role resolves the ambiguity and writes only that variable.
	targets, err = credentialWriteTargets(bindings, "auth_token")
	if err != nil {
		t.Fatalf("credentialWriteTargets(--role auth_token): %v", err)
	}
	if len(targets) != 1 || targets[0] != "ACME_TOKEN" {
		t.Errorf("targets = %v; want exactly [ACME_TOKEN]", targets)
	}
}

// TestCredentialWriteTargetsDeduplicatesSharedSourceVar covers the ordinary
// case: both roles interpolate the SAME variable, so there is no ambiguity and
// one write serves both.
func TestCredentialWriteTargetsDeduplicatesSharedSourceVar(t *testing.T) {
	bindings := []credentialBinding{
		{Role: "api_key", EnvKey: "ANTHROPIC_API_KEY", SourceVar: "ACME_KEY"},
		{Role: "auth_token", EnvKey: "ANTHROPIC_AUTH_TOKEN", SourceVar: "ACME_KEY"},
	}
	targets, err := credentialWriteTargets(bindings, "")
	if err != nil {
		t.Fatalf("credentialWriteTargets: %v", err)
	}
	if len(targets) != 1 || targets[0] != "ACME_KEY" {
		t.Errorf("targets = %v; want exactly [ACME_KEY]", targets)
	}
}

// TestCredentialWriteTargetsSurfacesRefusals: when no role can be rotated the
// caller must learn WHY, per role. A bare "nothing to do" is the failure mode
// the withdrawn command had.
func TestCredentialWriteTargetsSurfacesRefusals(t *testing.T) {
	bindings := []credentialBinding{
		{Role: "api_key", EnvKey: "ANTHROPIC_API_KEY", Refusal: "ANTHROPIC_API_KEY is a literal value"},
	}
	if _, err := credentialWriteTargets(bindings, ""); err == nil {
		t.Fatal("expected a refusal")
	} else if !strings.Contains(err.Error(), "literal value") {
		t.Errorf("error %q does not carry the per-role reason", err)
	}

	// A --role naming a role the provider does not declare is its own error.
	if _, err := credentialWriteTargets(bindings, "auth_token"); err == nil {
		t.Fatal("expected a refusal for an undeclared role")
	} else if !strings.Contains(err.Error(), "auth_token") {
		t.Errorf("error %q does not name the requested role", err)
	}
}

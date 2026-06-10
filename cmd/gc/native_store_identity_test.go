package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/identity"
)

func writeScopeIdentityFiles(t *testing.T, scopeRoot, metadataJSON, configYAML string) {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if metadataJSON != "" {
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadataJSON), 0o644); err != nil {
			t.Fatalf("write metadata.json: %v", err)
		}
	}
	if configYAML != "" {
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
			t.Fatalf("write config.yaml: %v", err)
		}
	}
}

func TestConfiguredScopeIdentity(t *testing.T) {
	tests := []struct {
		name         string
		metadataJSON string
		configYAML   string
		want         identity.ScopeIdentity
	}{
		{
			name:         "reads project_id and issue_prefix",
			metadataJSON: `{"backend":"dolt","project_id":"proj-vr"}`,
			configYAML:   "issue_prefix: vr\n",
			want:         identity.ScopeIdentity{ProjectID: "proj-vr", IssuePrefix: "vr"},
		},
		{
			name:         "missing metadata yields empty project_id",
			metadataJSON: "",
			configYAML:   "issue_prefix: vr\n",
			want:         identity.ScopeIdentity{IssuePrefix: "vr"},
		},
		{
			name:         "missing config yields empty prefix",
			metadataJSON: `{"backend":"dolt","project_id":"proj-vr"}`,
			configYAML:   "",
			want:         identity.ScopeIdentity{ProjectID: "proj-vr"},
		},
		{
			name:         "absent project_id key yields empty",
			metadataJSON: `{"backend":"dolt"}`,
			configYAML:   "issue_prefix: vr\n",
			want:         identity.ScopeIdentity{IssuePrefix: "vr"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scopeRoot := t.TempDir()
			writeScopeIdentityFiles(t, scopeRoot, tc.metadataJSON, tc.configYAML)
			got := configuredScopeIdentity(scopeRoot)
			if got != tc.want {
				t.Errorf("configuredScopeIdentity() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAssertNativeStoreIdentityNilStoreIsOpenedEmpty(t *testing.T) {
	scopeRoot := t.TempDir()
	writeScopeIdentityFiles(t, scopeRoot, `{"backend":"dolt","project_id":"proj-vr"}`, "issue_prefix: vr\n")

	var alerts []IdentityAlert
	sink := func(a IdentityAlert) { alerts = append(alerts, a) }

	result := assertNativeStoreIdentity(nil, scopeRoot, sink)
	if result.Class != identity.ClassOpenedEmpty {
		t.Fatalf("class = %q, want %q", result.Class, identity.ClassOpenedEmpty)
	}
	if !result.Degraded() {
		t.Fatal("opened-empty must be degraded")
	}
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	if alerts[0].ScopeRoot != scopeRoot {
		t.Errorf("alert scope = %q, want %q", alerts[0].ScopeRoot, scopeRoot)
	}
	if alerts[0].Result.Class != identity.ClassOpenedEmpty {
		t.Errorf("alert class = %q, want %q", alerts[0].Result.Class, identity.ClassOpenedEmpty)
	}
}

func TestAssertNativeStoreIdentityConfiguredEmptyStillAlerts(t *testing.T) {
	scopeRoot := t.TempDir()
	// No identity files at all: configured side is empty.
	var alerts []IdentityAlert
	sink := func(a IdentityAlert) { alerts = append(alerts, a) }

	result := assertNativeStoreIdentity(nil, scopeRoot, sink)
	if result.Class != identity.ClassConfiguredEmpty {
		t.Fatalf("class = %q, want %q", result.Class, identity.ClassConfiguredEmpty)
	}
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
}

func TestAssertNativeStoreIdentityNilSinkDoesNotPanic(t *testing.T) {
	scopeRoot := t.TempDir()
	// Degraded result with a nil sink must fall back to the default log sink
	// without panicking.
	result := assertNativeStoreIdentity(nil, scopeRoot, nil)
	if !result.Degraded() {
		t.Fatal("expected degraded result")
	}
}

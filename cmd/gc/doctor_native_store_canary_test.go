package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestNativeStoreCanaryIdentityCheck(t *testing.T) {
	t.Run("off when no scopes configured", func(t *testing.T) {
		cfg := &config.City{}
		r := newNativeStoreCanaryIdentityCheck(t.TempDir(), cfg).Run(nil)
		if r.Status != doctor.StatusOK {
			t.Fatalf("status = %v, want OK", r.Status)
		}
	})

	t.Run("nil config is OK", func(t *testing.T) {
		r := newNativeStoreCanaryIdentityCheck(t.TempDir(), nil).Run(nil)
		if r.Status != doctor.StatusOK {
			t.Fatalf("status = %v, want OK", r.Status)
		}
	})

	t.Run("canaried rig with complete identity passes", func(t *testing.T) {
		city := t.TempDir()
		rigPath := filepath.Join(city, "rigs", "vr")
		writeScopeIdentityFiles(t, rigPath, `{"backend":"dolt","project_id":"proj-vr"}`, "issue_prefix: vr\n")
		cfg := &config.City{
			Beads: config.BeadsConfig{NativeStoreCanaryScopes: []string{"vr"}},
			Rigs:  []config.Rig{{Name: "vr", Path: rigPath}},
		}
		r := newNativeStoreCanaryIdentityCheck(city, cfg).Run(nil)
		if r.Status != doctor.StatusOK {
			t.Fatalf("status = %v (%s), want OK", r.Status, r.Message)
		}
	})

	t.Run("canaried rig missing identity is advisory error", func(t *testing.T) {
		city := t.TempDir()
		rigPath := filepath.Join(city, "rigs", "vr")
		// config.yaml present but no metadata.json: project_id missing.
		writeScopeIdentityFiles(t, rigPath, "", "issue_prefix: vr\n")
		cfg := &config.City{
			Beads: config.BeadsConfig{NativeStoreCanaryScopes: []string{"vr"}},
			Rigs:  []config.Rig{{Name: "vr", Path: rigPath}},
		}
		r := newNativeStoreCanaryIdentityCheck(city, cfg).Run(nil)
		if r.Status != doctor.StatusError {
			t.Fatalf("status = %v, want Error", r.Status)
		}
		if r.Severity != doctor.SeverityAdvisory {
			t.Errorf("severity = %v, want Advisory", r.Severity)
		}
		if len(r.Details) == 0 {
			t.Error("expected details listing the incomplete scope")
		}
	})

	t.Run("unresolvable scope name reported", func(t *testing.T) {
		city := t.TempDir()
		cfg := &config.City{
			Beads: config.BeadsConfig{NativeStoreCanaryScopes: []string{"ghost"}},
		}
		r := newNativeStoreCanaryIdentityCheck(city, cfg).Run(nil)
		if r.Status != doctor.StatusError {
			t.Fatalf("status = %v, want Error", r.Status)
		}
	})

	t.Run("city scope resolves to city root", func(t *testing.T) {
		city := t.TempDir()
		writeScopeIdentityFiles(t, city, `{"backend":"dolt","project_id":"proj-hq"}`, "issue_prefix: hq\n")
		cfg := &config.City{
			Workspace: config.Workspace{Name: "hq"},
			Beads:     config.BeadsConfig{NativeStoreCanaryScopes: []string{"hq"}},
		}
		r := newNativeStoreCanaryIdentityCheck(city, cfg).Run(nil)
		if r.Status != doctor.StatusOK {
			t.Fatalf("status = %v (%s), want OK", r.Status, r.Message)
		}
	})
}

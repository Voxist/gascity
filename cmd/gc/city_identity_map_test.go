package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCityIdentityMapEntry(t *testing.T) {
	t.Run("returns_project_id_for_registered_rig", func(t *testing.T) {
		cityDir := t.TempDir()
		rigDir := filepath.Join(cityDir, "my-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		cityTOML := `
[[rigs]]
name = "my-rig"

[identity_map]
my-rig = { project_id = "canonical-uuid-1234", project = "my-project" }
`
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok, err := readCityIdentityMapEntry(cityDir, rigDir)
		if err != nil {
			t.Fatalf("readCityIdentityMapEntry: %v", err)
		}
		if !ok {
			t.Fatal("ok=false, want true")
		}
		if got != "canonical-uuid-1234" {
			t.Fatalf("project_id = %q, want %q", got, "canonical-uuid-1234")
		}
	})

	t.Run("returns_false_when_no_identity_map_block", func(t *testing.T) {
		cityDir := t.TempDir()
		rigDir := filepath.Join(cityDir, "my-rig")
		cityTOML := `
[[rigs]]
name = "my-rig"
`
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
			t.Fatal(err)
		}
		_, ok, err := readCityIdentityMapEntry(cityDir, rigDir)
		if err != nil {
			t.Fatalf("readCityIdentityMapEntry: %v", err)
		}
		if ok {
			t.Fatal("ok=true, want false (no identity_map)")
		}
	})

	t.Run("returns_false_when_rig_not_in_identity_map", func(t *testing.T) {
		cityDir := t.TempDir()
		rigDir := filepath.Join(cityDir, "my-rig")
		cityTOML := `
[[rigs]]
name = "my-rig"

[identity_map]
other-rig = { project_id = "other-uuid" }
`
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
			t.Fatal(err)
		}
		_, ok, err := readCityIdentityMapEntry(cityDir, rigDir)
		if err != nil {
			t.Fatalf("readCityIdentityMapEntry: %v", err)
		}
		if ok {
			t.Fatal("ok=true, want false (rig not in identity_map)")
		}
	})

	t.Run("returns_false_when_city_toml_missing", func(t *testing.T) {
		cityDir := t.TempDir()
		rigDir := filepath.Join(cityDir, "my-rig")
		_, ok, err := readCityIdentityMapEntry(cityDir, rigDir)
		if err != nil {
			t.Fatalf("readCityIdentityMapEntry: %v", err)
		}
		if ok {
			t.Fatal("ok=true, want false (city.toml missing)")
		}
	})

	t.Run("uses_explicit_path_field_over_default", func(t *testing.T) {
		cityDir := t.TempDir()
		customDir := t.TempDir()
		cityTOML := `
[[rigs]]
name = "my-rig"
path = "` + customDir + `"

[identity_map]
my-rig = { project_id = "custom-path-uuid" }
`
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok, err := readCityIdentityMapEntry(cityDir, customDir)
		if err != nil {
			t.Fatalf("readCityIdentityMapEntry: %v", err)
		}
		if !ok {
			t.Fatal("ok=false, want true (explicit path field)")
		}
		if got != "custom-path-uuid" {
			t.Fatalf("project_id = %q, want %q", got, "custom-path-uuid")
		}
	})
}

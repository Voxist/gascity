package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestLoadWarnsOnRetiredProxiedOptInKeys pins the retirement of the fork's
// [beads] proxied / proxy_pool_size / proxy_idle_timeout opt-in and its
// [storehealth] patrol section (ga-s69mk.3). A city.toml that still carries
// them must load with one retired-key warning per key, never an unknown-field
// error, so a deployed city keeps booting until an operator drops the lines.
func TestLoadWarnsOnRetiredProxiedOptInKeys(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[beads]
proxied = false
proxy_pool_size = 2
proxy_idle_timeout = "5m"

[storehealth]
enabled = true
interval = "30s"
consecutive_fails = 3
reap_cooldown = "10m"
write_probe_interval = "10m"
`)
	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	for _, key := range []string{
		"beads.proxied",
		"beads.proxy_pool_size",
		"beads.proxy_idle_timeout",
		"storehealth",
		"storehealth.enabled",
		"storehealth.interval",
		"storehealth.consecutive_fails",
		"storehealth.reap_cooldown",
		"storehealth.write_probe_interval",
	} {
		found := false
		for _, w := range prov.Warnings {
			if strings.Contains(w, `"`+key+`"`) && IsRetiredKeyWarning(w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no retired-key warning for %q; warnings = %q", key, prov.Warnings)
		}
	}
	for _, w := range prov.Warnings {
		if strings.Contains(w, "unknown field") {
			t.Errorf("retired key reported as an unknown field: %q", w)
		}
	}
}

// TestRetiredProxiedOptInKeysPointAtReplacement keeps the migration note on
// each retired [beads] key pointing at the upstream bd-owned replacement.
func TestRetiredProxiedOptInKeysPointAtReplacement(t *testing.T) {
	for _, key := range []string{"beads.proxied", "beads.proxy_pool_size", "beads.proxy_idle_timeout"} {
		rk, ok := retiredKeys[key]
		if !ok {
			t.Errorf("%q is not registered in retiredKeys", key)
			continue
		}
		if !strings.Contains(rk.Note, "gc beads city migrate-proxied") || !strings.Contains(rk.Note, "proxied_idle_timeout") {
			t.Errorf("%q note = %q, want it to name `gc beads city migrate-proxied` and proxied_idle_timeout", key, rk.Note)
		}
	}
}

// TestRewriteDropsRetiredProxiedOptInKeys pins that a city.toml still carrying
// the retired keys can be rewritten (suspend, rig edits, gc migrate, doctor
// fixes all round-trip through this guard), and that the rewrite drops them:
// they are ignored on load, so losing them is the intended outcome.
func TestRewriteDropsRetiredProxiedOptInKeys(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	original := []byte(`[workspace]
name = "test-city"

[beads]
proxied = false
proxy_pool_size = 2

[storehealth]
interval = "30s"
`)
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Workspace.Name = "renamed-city"

	if err := WriteCityAndRigSiteBindingsForEdit(fsys.OSFS{}, cityPath, cfg); err != nil {
		t.Fatalf("WriteCityAndRigSiteBindingsForEdit refused a city.toml carrying only retired keys: %v", err)
	}
	data, err := os.ReadFile(cityPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), `name = "renamed-city"`) {
		t.Fatalf("rewrite did not apply the edit:\n%s", data)
	}
	for _, gone := range []string{"proxied", "proxy_pool_size", "storehealth"} {
		if strings.Contains(string(data), gone) {
			t.Errorf("rewritten city.toml still carries retired %q:\n%s", gone, data)
		}
	}
}

// TestRewriteStillRefusesUnknownKeyBesideRetiredKeys keeps the key-loss guard
// intact for genuinely unknown keys when retired keys are also present.
func TestRewriteStillRefusesUnknownKeyBesideRetiredKeys(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	original := []byte(`[workspace]
name = "test-city"

[beads]
proxied = false
future_unknown_knob = "keep-me"
`)
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Workspace.Name = "renamed-city"

	err = WriteCityAndRigSiteBindingsForEdit(fsys.OSFS{}, cityPath, cfg)
	if err == nil {
		t.Fatal("WriteCityAndRigSiteBindingsForEdit succeeded, want refusal for the unknown key")
	}
	if !strings.Contains(err.Error(), "beads.future_unknown_knob") {
		t.Fatalf("error = %v, want mention of beads.future_unknown_knob", err)
	}
	if strings.Contains(err.Error(), "beads.proxied") {
		t.Fatalf("error = %v, must not list the retired beads.proxied key", err)
	}
	current, readErr := os.ReadFile(cityPath)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if !bytes.Equal(current, original) {
		t.Fatalf("city.toml was rewritten despite refusal:\n%s", current)
	}
}

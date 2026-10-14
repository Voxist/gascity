package config

import (
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

package providergov

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func boolPtr(b bool) *bool { return &b }

func TestAccountsFromConfigSelectsEnabledProviders(t *testing.T) {
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{
			"claude2": {
				QuotaMonitor:     boolPtr(true),
				MonitorConfigDir: "/var/monitor-claude2",
			},
			"claude": {
				QuotaMonitor:     boolPtr(true),
				MonitorConfigDir: "/var/monitor-claude",
			},
			"zai":      {},                             // not enabled
			"disabled": {QuotaMonitor: boolPtr(false)}, // explicit off
		},
	}
	accounts, err := AccountsFromConfig(cfg)
	if err != nil {
		t.Fatalf("AccountsFromConfig: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v, want 2 entries", accounts)
	}
	// Deterministic name order.
	if accounts[0].Name != "claude" || accounts[1].Name != "claude2" {
		t.Errorf("order = [%s %s], want [claude claude2]", accounts[0].Name, accounts[1].Name)
	}
	if accounts[0].ConfigDir != "/var/monitor-claude" {
		t.Errorf("ConfigDir = %q, want /var/monitor-claude", accounts[0].ConfigDir)
	}
}

func TestAccountsFromConfigExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{
			"claude": {
				QuotaMonitor:     boolPtr(true),
				MonitorConfigDir: "~/.gc/monitor-claude",
			},
		},
	}
	accounts, err := AccountsFromConfig(cfg)
	if err != nil {
		t.Fatalf("AccountsFromConfig: %v", err)
	}
	want := filepath.Join(home, ".gc", "monitor-claude")
	if accounts[0].ConfigDir != want {
		t.Errorf("ConfigDir = %q, want %q", accounts[0].ConfigDir, want)
	}
}

func TestAccountsFromConfigRejectsMissingDir(t *testing.T) {
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{
			"claude": {QuotaMonitor: boolPtr(true)},
		},
	}
	_, err := AccountsFromConfig(cfg)
	if err == nil {
		t.Fatal("expected error for quota_monitor without monitor_config_dir")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error %q does not name the provider", err)
	}
}

func TestAccountsFromConfigNilConfig(t *testing.T) {
	accounts, err := AccountsFromConfig(nil)
	if err != nil {
		t.Fatalf("AccountsFromConfig(nil): %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("accounts = %+v, want empty", accounts)
	}
}

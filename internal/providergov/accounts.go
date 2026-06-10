package providergov

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// AccountsFromConfig extracts the monitor-enabled accounts from a city
// config: every [providers.<name>] block with quota_monitor = true.
// monitor_config_dir is required for enabled providers and a leading
// "~/" (or bare "~") is expanded against the current home directory.
// Results are sorted by name for deterministic polling and output order.
func AccountsFromConfig(cfg *config.City) ([]Account, error) {
	if cfg == nil {
		return nil, nil
	}
	var accounts []Account
	for name, spec := range cfg.Providers {
		if spec.QuotaMonitor == nil || !*spec.QuotaMonitor {
			continue
		}
		dir := strings.TrimSpace(spec.MonitorConfigDir)
		if dir == "" {
			return nil, fmt.Errorf("provider %q sets quota_monitor = true but no monitor_config_dir", name)
		}
		expanded, err := expandHome(dir)
		if err != nil {
			return nil, fmt.Errorf("provider %q: expanding monitor_config_dir %q: %w", name, dir, err)
		}
		accounts = append(accounts, Account{Name: name, ConfigDir: expanded})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return accounts, nil
}

// expandHome expands a leading "~" or "~/" against the home directory.
// Other paths pass through unchanged.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

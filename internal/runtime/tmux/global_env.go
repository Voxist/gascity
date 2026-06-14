package tmux

import (
	"fmt"
	"strings"
)

// SetGlobalEnvironment sets an environment variable in the tmux server global
// environment. The global env is inherited by new sessions and by sessions that
// do not have the variable set explicitly at the session level.
func (t *Tmux) SetGlobalEnvironment(key, value string) error {
	if key == "" {
		return fmt.Errorf("environment variable key must not be empty")
	}
	_, err := t.run("set-environment", "-g", key, value)
	return err
}

// GetGlobalEnvironment reads an environment variable from the tmux server
// global environment.
func (t *Tmux) GetGlobalEnvironment(key string) (string, error) {
	out, err := t.run("show-environment", "-g", key)
	if err != nil {
		return "", err
	}
	// Output format: KEY=value
	parts := strings.SplitN(out, "=", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("unexpected environment format for %s: %q", key, out)
	}
	return parts[1], nil
}

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/processenv"
)

// RotateTmux is the narrow tmux interface needed by rotateProviderKey.
// Satisfied by *tmux.Tmux.
type RotateTmux interface {
	SetGlobalEnvironment(key, value string) error
	ListSessions() ([]string, error)
	GetEnvironment(session, key string) (string, error)
	SetEnvironment(session, key, value string) error
}

// RotateResult summarizes what rotateProviderKey changed (or would change in dry-run).
type RotateResult struct {
	GlobalVarsUpdated []string // source var names written to tmux global env
	SessionsUpdated   []string // session names whose per-session env was refreshed
}

// ProviderEnv is the narrow provider view needed by the core rotation logic.
type ProviderEnv struct {
	// Env is the raw ProviderSpec.Env map; values may contain ${VAR} interpolation refs.
	Env map[string]string
}

// rotateProviderKey propagates newKey into the tmux server global env and into
// every live session that is using providerName, without requiring a tmux
// kill-server. It updates:
//   - the source vars (${VAR} refs in spec.Env values) in the global env
//   - the source vars and expanded keys (spec.Env keys) in each matching session
//
// When dryRun is true, no tmux state is written; RotateResult describes what
// would have changed.
func rotateProviderKey(_ context.Context, providerName, newKey string, tm RotateTmux, spec ProviderEnv, dryRun bool) (RotateResult, error) {
	sourceVars := processenv.ProviderSourceVars(spec.Env)
	sort.Strings(sourceVars)

	// Collect only the keys whose values contain ${...} refs — static-literal
	// keys (e.g. ANTHROPIC_BASE_URL=https://...) must not be overwritten.
	expandedVars := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		if strings.Contains(v, "${") {
			expandedVars = append(expandedVars, k)
		}
	}
	sort.Strings(expandedVars)

	result := RotateResult{}

	// Update the tmux server global env for each source var.
	for _, sv := range sourceVars {
		if !dryRun {
			if err := tm.SetGlobalEnvironment(sv, newKey); err != nil {
				return result, fmt.Errorf("setting global env %s: %w", sv, err)
			}
		}
		result.GlobalVarsUpdated = append(result.GlobalVarsUpdated, sv)
	}

	// Enumerate live sessions and refresh those belonging to providerName.
	sessions, err := tm.ListSessions()
	if err != nil {
		return result, fmt.Errorf("listing sessions: %w", err)
	}
	sort.Strings(sessions)

	for _, sess := range sessions {
		provider, err := tm.GetEnvironment(sess, "GC_PROVIDER")
		if err != nil || provider != providerName {
			continue
		}

		if !dryRun {
			// Update source vars in the session.
			for _, sv := range sourceVars {
				if err := tm.SetEnvironment(sess, sv, newKey); err != nil {
					return result, fmt.Errorf("setting session %s env %s: %w", sess, sv, err)
				}
			}
			// Update expanded keys (the injected vars) in the session.
			for _, ev := range expandedVars {
				if err := tm.SetEnvironment(sess, ev, newKey); err != nil {
					return result, fmt.Errorf("setting session %s env %s: %w", sess, ev, err)
				}
			}
		}
		result.SessionsUpdated = append(result.SessionsUpdated, sess)
	}

	return result, nil
}

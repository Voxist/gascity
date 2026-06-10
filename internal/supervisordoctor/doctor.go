// Package supervisordoctor implements the cheap doctor-check subset the
// supervisor evaluates on startup and every [doctor] supervisor_interval
// (city-scale architecture plan item 1.9). Running these checks at
// supervisor cadence — independent of any one city's tick or store path —
// closes the detection-at-human-cadence hole that produced incidents 5 and
// 11: an in-band sentinel cannot detect the failures that disable it.
//
// Every check is mechanical (durations, counts, filesystem facts) so the
// package holds no judgment calls. All inputs are passed in; the package
// performs no I/O of its own except the filesystem walk in
// CheckAgentConfigIsolation, which is a read-only lstat traversal. The
// supervisor layer gathers inputs and publishes doctor.alert events for
// each returned Alert.
package supervisordoctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Check names, used as the doctor.alert "check" field.
const (
	CheckNameTickAge              = "tick_age"
	CheckNameAgentConfigIsolation = "agent_config_isolation"
	CheckNameS6ConnectionCeiling  = "s6_connection_ceiling"
)

// Alert is one red doctor finding.
type Alert struct {
	// Check is the check name (one of the Check* constants).
	Check string
	// Subject is the optional subject the alert concerns (city, scope, path).
	Subject string
	// Detail is the human-readable red condition.
	Detail string
}

// TickAgeInput holds one city's heartbeat staleness facts.
type TickAgeInput struct {
	// City is the city name (alert subject).
	City string
	// LastTickAge is now minus the timestamp of the most recent
	// controller.tick_completed event. A zero/negative value means "no
	// heartbeat seen yet" and is treated as not-red (the city may be
	// starting; the next interval re-checks).
	LastTickAge time.Duration
	// PatrolInterval is the city's [daemon] patrol_interval.
	PatrolInterval time.Duration
	// HasHeartbeat is false when no controller.tick_completed event exists
	// yet for the city; the check is skipped (not red) in that case.
	HasHeartbeat bool
}

// tickAgeRedMultiple is the staleness threshold: a heartbeat older than
// this many patrol intervals is red.
const tickAgeRedMultiple = 3

// CheckTickAgeFor returns an Alert when a city's last heartbeat is older
// than tickAgeRedMultiple patrol intervals. Returns nil when healthy, when
// no heartbeat exists yet, or when the patrol interval is non-positive
// (cannot compute a threshold).
func CheckTickAgeFor(in TickAgeInput) *Alert {
	if !in.HasHeartbeat || in.PatrolInterval <= 0 {
		return nil
	}
	threshold := time.Duration(tickAgeRedMultiple) * in.PatrolInterval
	if in.LastTickAge <= threshold {
		return nil
	}
	return &Alert{
		Check:   CheckNameTickAge,
		Subject: in.City,
		Detail: fmt.Sprintf("last controller tick %s ago exceeds %d×patrol (%s); controller heartbeat is stale",
			in.LastTickAge.Round(time.Second), tickAgeRedMultiple, threshold),
	}
}

// S6Input holds the connection-ceiling facts for one city.
type S6Input struct {
	// City is the city name (alert subject).
	City string
	// Scopes is the number of store scopes (city + bound rigs).
	Scopes int
	// PoolSize is the per-scope managed connection pool size (the proxy
	// pool size / connections each scope can hold). The ceiling models
	// scopes×(pool+1) total connections.
	PoolSize int
	// MaxConnections is the managed dolt @@max_connections value from
	// config. A non-positive value skips the check (the ceiling cannot be
	// evaluated without it).
	MaxConnections int
}

// s6CeilingFraction is the fraction of @@max_connections the projected
// connection demand must stay under (0.8×, plan items 1.9/2.7).
const s6CeilingFraction = 0.8

// CheckS6ConnectionCeiling returns an Alert when the projected connection
// demand scopes×(pool+1) exceeds s6CeilingFraction×max_connections.
// Returns nil when within budget or when max_connections is unknown.
func CheckS6ConnectionCeiling(in S6Input) *Alert {
	if in.MaxConnections <= 0 || in.Scopes <= 0 {
		return nil
	}
	projected := in.Scopes * (in.PoolSize + 1)
	ceiling := s6CeilingFraction * float64(in.MaxConnections)
	if float64(projected) <= ceiling {
		return nil
	}
	return &Alert{
		Check:   CheckNameS6ConnectionCeiling,
		Subject: in.City,
		Detail: fmt.Sprintf("projected connection demand %d (%d scopes × (pool %d + 1)) exceeds 0.8×max_connections (%.0f of %d)",
			projected, in.Scopes, in.PoolSize, ceiling, in.MaxConnections),
	}
}

// CheckAgentConfigIsolation returns an Alert for each agent config-dir root
// that contains a symlink escaping the root (incident 11: the
// agent plugins dir symlinking out to the operator's ~/.claude/plugins,
// bloating the fleet's MCP process count). The walk is a read-only lstat
// traversal; it never follows the offending symlinks. roots that do not
// exist are skipped (not red).
func CheckAgentConfigIsolation(roots []string) []Alert {
	var alerts []Alert
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if escaping := symlinkEscapesRoot(abs); escaping != "" {
			alerts = append(alerts, Alert{
				Check:   CheckNameAgentConfigIsolation,
				Subject: abs,
				Detail:  fmt.Sprintf("symlink %q escapes the agent config dir; remove it (incident 11 MCP fleet bloat)", escaping),
			})
		}
	}
	return alerts
}

// symlinkEscapesRoot walks root and returns the path of the first symlink
// whose resolved target lies outside root, or "" when none escape. A
// missing root yields "" (skipped). The walk does not descend through
// symlinked directories (filepath.WalkDir uses lstat), so a self-contained
// internal symlink tree is fine and only escaping links are flagged.
func symlinkEscapesRoot(root string) string {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return ""
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootResolved = root
	}
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || found != "" {
			return nil
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return nil
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		resolved, err := filepath.EvalSymlinks(target)
		if err != nil {
			// A dangling symlink cannot be confirmed in-root; treat its
			// literal target for the containment check.
			resolved = filepath.Clean(target)
		}
		if !pathWithin(resolved, rootResolved) {
			found = path
		}
		return nil
	})
	return found
}

// pathWithin reports whether child is root itself or lives under root.
func pathWithin(child, root string) bool {
	child = filepath.Clean(child)
	root = filepath.Clean(root)
	if child == root {
		return true
	}
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

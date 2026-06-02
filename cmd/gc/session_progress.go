// session_progress.go — progress-aware session stall predicate (ADR-0013 A1 M3b).
//
// A desired, alive session that has stopped progressing has likely parked (e.g.
// its turn ended on a provider auth error) and will not self-recover. This file
// provides the stall predicate and K=3 episode tracking that gates forced
// restarts and caps escalation alert volume.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/google/uuid"
)

// sessionProgressStalled reports whether a desired, alive session has stopped
// making progress and should be recycled with a fresh restart.
//
// Returns true only when ALL hold:
//   - threshold > 0: opt-in; zero disables the feature.
//   - !holdsClaim: a session holding in-progress work is the stall-reaper's
//     domain; this targets the claim-less parked case the reaper cannot see.
//   - providerHealthy: never recycle while the provider cannot serve; composes
//     with the provider-health respawn gate (move 3a, vp-0a3).
//   - !exempt: not attached, not awaiting interaction, not in startup grace.
//   - lastProgress is known and older than threshold.
func sessionProgressStalled(threshold time.Duration, holdsClaim, providerHealthy, exempt bool, lastProgress, now time.Time) bool {
	if threshold <= 0 || holdsClaim || !providerHealthy || exempt {
		return false
	}
	if lastProgress.IsZero() {
		return false
	}
	return now.Sub(lastProgress) > threshold
}

// --- K=3 episode state ---

// progressEpisodeState tracks consecutive stall-restarts for one session.
// A new episode starts when the session is first recycled due to stale progress;
// the episode closes (counter resets) when progress advances past LastRestartAt.
type progressEpisodeState struct {
	// EpisodeID is a UUID assigned when the session enters a new stall episode.
	EpisodeID string
	// RestartCount is the number of stall-triggered restarts in this episode.
	RestartCount int
	// LastRestartAt records when the most recent stall restart was requested.
	LastRestartAt time.Time
	// EscalationSent is true after the K=3 alert has been emitted for this
	// episode. Cleared when progress advances and the episode resets.
	EscalationSent bool
}

// progressEpisodeTracker is reconciler-scoped, persistent across ticks.
// It lives on CityRuntime alongside sessionDrains and providerHealthGate.
type progressEpisodeTracker struct {
	mu       sync.Mutex
	episodes map[string]*progressEpisodeState // keyed by session bead ID
}

// newProgressEpisodeTracker returns an empty tracker.
func newProgressEpisodeTracker() *progressEpisodeTracker {
	return &progressEpisodeTracker{episodes: make(map[string]*progressEpisodeState)}
}

const progressEscalationK = 3

// recordStallRestart notes a stall-triggered restart for sessionID. If this
// is the first restart in a new episode, a fresh EpisodeID is assigned. The
// returned (count, escalate) tells the caller how many restarts have happened
// and whether to emit the K=3 escalation alert.
func (t *progressEpisodeTracker) recordStallRestart(sessionID string, now time.Time) (count int, escalate bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.episodeFor(sessionID)
	if s.EpisodeID == "" {
		s.EpisodeID = uuid.New().String()
	}
	s.RestartCount++
	s.LastRestartAt = now
	if s.RestartCount >= progressEscalationK && !s.EscalationSent {
		s.EscalationSent = true
		return s.RestartCount, true
	}
	return s.RestartCount, false
}

// isEscalated reports whether auto-restart has been suppressed for sessionID
// (K=3 already reached). Safe to call from the reconciler's hot path.
func (t *progressEpisodeTracker) isEscalated(sessionID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.episodes[sessionID]
	if !ok {
		return false
	}
	return s.RestartCount >= progressEscalationK
}

// advanceProgress clears the stall episode for sessionID when progress_at has
// advanced past the most recent stall restart. This lets the session try again
// from zero after it recovers.
func (t *progressEpisodeTracker) advanceProgress(sessionID string, progressAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.episodes[sessionID]
	if !ok {
		return
	}
	if !s.LastRestartAt.IsZero() && progressAt.After(s.LastRestartAt) {
		delete(t.episodes, sessionID) // reset episode; next stall opens a fresh one
	}
}

// resetOnReclaim clears the stall episode when a session successfully claims
// a work bead (Component 4 of plan-vc-uqnu9). Prevents stale episode state
// from penalizing a session that recovered on its own between ticks.
func (t *progressEpisodeTracker) resetOnReclaim(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.episodes, sessionID)
}

func (t *progressEpisodeTracker) episodeFor(sessionID string) *progressEpisodeState {
	if s, ok := t.episodes[sessionID]; ok {
		return s
	}
	s := &progressEpisodeState{}
	t.episodes[sessionID] = s
	return s
}

// episodeIDForSession returns the current episode ID or "" if none open.
// Safe to call after recordStallRestart has set the ID.
func (t *progressEpisodeTracker) episodeIDForSession(sessionID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.episodes[sessionID]; ok {
		return s.EpisodeID
	}
	return ""
}

// emitProgressStallEscalationAlert writes the K=3 escalation alert to stdout
// and records a ProgressStallEscalation event. Called once per episode.
func emitProgressStallEscalationAlert(
	rec events.Recorder,
	stdout io.Writer,
	sessionName, sessionID, template, episodeID string,
	firstStallAt time.Time,
	restartCount int,
) {
	msg := fmt.Sprintf(
		"Progress-stall escalation: session=%s episode=%s template=%s since=%s restarts=%d. "+
			"Auto-restart suspended after %d consecutive stall cycles. "+
			"Manual intervention required: gc session reset %s",
		sessionName, episodeID, template, firstStallAt.UTC().Format(time.RFC3339),
		restartCount, progressEscalationK, sessionName,
	)
	fmt.Fprintln(stdout, msg) //nolint:errcheck
	if rec == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"session_name":   sessionName,
		"session_id":     sessionID,
		"template":       template,
		"episode_id":     episodeID,
		"first_stall_at": firstStallAt.UTC().Format(time.RFC3339),
		"restart_count":  restartCount,
	})
	rec.Record(events.Event{
		Type:    events.ProgressStallEscalation,
		Ts:      time.Now().UTC(),
		Actor:   "gc",
		Subject: sessionName,
		Message: msg,
		Payload: payload,
	})
}

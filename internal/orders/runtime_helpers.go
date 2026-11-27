package orders

import (
	"log"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

var runtimeHelpersLogf = log.Printf

// LastRunAcross returns a LastRunFunc reporting the most recent run time for a
// named order across a federation of order front doors (the dispatcher/CLI
// city + rig scopes). Each *Store performs its own MIXED orders+graph LastRun
// read (unioning its orders leg with its graph leg); the max across scopes wins.
// A per-scope error aborts and propagates. nil entries are skipped.
func LastRunAcross(stores []*Store) LastRunFunc {
	return func(name string) (time.Time, error) {
		var latest time.Time
		for _, s := range stores {
			if s == nil {
				continue
			}
			last, err := s.LastRun(name)
			if err != nil {
				return time.Time{}, err
			}
			if last.After(latest) {
				latest = last
			}
		}
		return latest, nil
	}
}

// LastRunFuncWithEventFallback wraps fn with an events.Provider fallback.
// When fn returns a zero time (no tracking beads found — e.g. after the
// tracking beads were purged), it queries the event bus for the most recent
// order.fired event whose Subject matches the order's scoped name and
// returns its timestamp. A nil ep disables the fallback and the wrapped
// fn's result is returned unchanged. Event bus errors are non-fatal:
// when the provider fails the zero time is returned so the order appears
// unrun rather than permanently blocked.
//
// A provider that implements events.LatestMatchProvider (the file recorder)
// is asked for the newest match only: it reads the active log's tail, then
// the rotated history (archives and in-flight rotating-* files) newest seq
// window first, stopping at the first segment that holds one. List would
// gunzip and decode every retained archive oldest-first to take a max, so an
// order with no tracking bead would cost a walk of the whole event history
// per dispatcher start. Skipped history segments and a failed read of the
// active log are logged; the read error still fails open. Other providers
// keep the List path.
func LastRunFuncWithEventFallback(fn LastRunFunc, ep events.Provider) LastRunFunc {
	if ep == nil {
		return fn
	}
	return func(name string) (time.Time, error) {
		last, err := fn(name)
		if err != nil {
			return time.Time{}, err
		}
		if !last.IsZero() {
			return last, nil
		}
		filter := events.Filter{
			Type:    events.OrderFired,
			Subject: name,
		}
		if lp, ok := ep.(events.LatestMatchProvider); ok {
			e, found, warnings, latestErr := lp.LatestMatch(filter)
			for _, w := range warnings {
				runtimeHelpersLogf("orders: last-run event fallback for %q: %s", name, w)
			}
			if latestErr != nil {
				runtimeHelpersLogf("orders: last-run event fallback for %q: %v (treating the order as unrun)", name, latestErr)
				return time.Time{}, nil
			}
			if !found {
				return time.Time{}, nil
			}
			return e.Ts, nil
		}
		evts, listErr := ep.List(filter)
		if listErr != nil {
			return time.Time{}, nil
		}
		var latest time.Time
		for _, e := range evts {
			if e.Ts.After(latest) {
				latest = e.Ts
			}
		}
		return latest, nil
	}
}

// CursorAcross returns a CursorFunc merging the event seq cursor for a named
// order across a federation of order front doors. Each *Store performs its own
// MIXED orders+graph Cursor read; the max seq across scopes wins. nil entries
// are skipped.
func CursorAcross(stores []*Store) CursorFunc {
	return func(name string) uint64 {
		var latest uint64
		for _, s := range stores {
			if s == nil {
				continue
			}
			if seq := uint64(s.Cursor(name)); seq > latest {
				latest = seq
			}
		}
		return latest
	}
}

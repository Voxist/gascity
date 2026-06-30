package api

import (
	"context"
	"time"
)

// Warm-StatusView tuning (vp-e0hv). The /v0/status full body costs ~28s to
// build on a loaded city because its work-count / store-health sub-queries are
// not effectively cancellable. Building it on every request made cold
// `gc status` time out and fall back to the slow local probe. Instead we serve
// the last-built body (stale-while-revalidate) and rebuild off the request
// path; only the first build after a (re)start or long idle is synchronous.
var (
	// statusWarmServeMaxAge bounds how stale a warm body may be and still be
	// served without forcing a synchronous rebuild. Generous: a status overview
	// minutes old beats a ~28s block, and a refresh is kicked the moment it
	// ages past statusWarmRefreshAfter.
	statusWarmServeMaxAge = 5 * time.Minute
	// statusWarmRefreshAfter is the age past which serving a warm body also
	// triggers a background rebuild, so the served body tracks reality closely
	// on an actively-polled city.
	statusWarmRefreshAfter = 5 * time.Second
	// statusWarmBuildTimeout backstops a single build so a wedged store cannot
	// pin the build forever. Larger than the observed ~28s so a healthy build is
	// never cut off. buildStatusBody's own sub-timeouts are tighter.
	statusWarmBuildTimeout = 60 * time.Second
)

// statusWarmEntry is a built status body and the time it was built.
type statusWarmEntry struct {
	body    StatusBody
	builtAt time.Time
}

// warmStatusBody returns the current warm entry for the full or lite variant.
func (s *Server) warmStatusBody(lite bool) (*statusWarmEntry, bool) {
	s.statusWarmMu.Lock()
	defer s.statusWarmMu.Unlock()
	e := s.statusWarmFull
	if lite {
		e = s.statusWarmLite
	}
	if e == nil {
		return nil, false
	}
	return e, true
}

// setWarmStatusBody records a freshly built body as the warm entry.
func (s *Server) setWarmStatusBody(lite bool, body StatusBody, builtAt time.Time) {
	s.statusWarmMu.Lock()
	defer s.statusWarmMu.Unlock()
	e := &statusWarmEntry{body: body, builtAt: builtAt}
	if lite {
		s.statusWarmLite = e
	} else {
		s.statusWarmFull = e
	}
}

// buildAndStoreStatus builds the status body and records it as the warm entry
// (and seeds the ≤2s response cache for high-frequency bursts). The build is
// single-flighted per variant via statusBuildSF, so a burst of cold requests —
// or a cold request racing a background refresh — shares ONE build rather than
// stampeding the store with abandoned scans. Blocks until the build completes;
// callers that must not block use refreshStatusBodyAsync.
func (s *Server) buildAndStoreStatus(lite bool) StatusBody {
	key := "status"
	if lite {
		key = "status?lite"
	}
	v, _, _ := s.statusBuildSF.Do(key, func() (any, error) {
		// Detached from any request ctx and longer-lived than backgroundCtx
		// (30s) — a status build can legitimately take ~28s on a loaded city,
		// and completing it warms the cache even if the triggering client left.
		ctx, cancel := context.WithTimeout(context.Background(), statusWarmBuildTimeout)
		defer cancel()
		body := s.buildStatusBody(ctx, lite)
		now := nowFunc()
		s.setWarmStatusBody(lite, body, now)
		s.storeResponse(key, responseCacheTimeBucket(now), body)
		return body, nil
	})
	body, _ := v.(StatusBody)
	return body
}

// refreshStatusBodyAsync rebuilds the status body off the request path (used
// when a warm body exists but is aging), so the request is never blocked on the
// rebuild. statusBuildAsyncHook is the spawn point, overridable in tests to run
// the build inline and deterministically.
func (s *Server) refreshStatusBodyAsync(lite bool) {
	statusBuildAsyncHook(func() { s.buildAndStoreStatus(lite) })
}

// statusBuildAsyncHook spawns the background build. Tests override it to run the
// build synchronously without a real goroutine.
var statusBuildAsyncHook = func(build func()) { go build() }

// nowFunc is time.Now, indirected so tests can pin build timestamps.
var nowFunc = time.Now

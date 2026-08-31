package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// TestHandleStatusWarmServeReportsWarmBodyAgeWhenStoreIsFresh guards the
// staleness signal on the warm-serve path.
//
// statusWarmServeMaxAge is 5m, but the CLI raises its staleness banner at
// cacheAgeBannerThresholdSeconds (30s). So a minutes-old warm body can be
// served off a freshly reconciled store; if CacheAgeS reported only the store
// snapshot age it would come back ~0 and silently suppress the banner on
// exactly the stale view the operator needs told about. The warm path must
// report the GREATER of the two ages, the same rule the SWR path applies.
func TestHandleStatusWarmServeReportsWarmBodyAgeWhenStoreIsFresh(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Nanosecond // force past the bucket cache
	oldFloor := statusResponseTTLFloor
	statusResponseTTLFloor = 0 // force past the TTL-floor cache
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
	})

	// A FRESH store snapshot: the store-side age signal is ~0, so anything the
	// header reports above it can only have come from the warm body's own age.
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(SetLivenessClockForTest(&clock.Fake{Time: now}))

	state := newFakeState(t)
	state.stores["myrig"] = beads.NewMemStore()
	state.cityBeadStore = stubLivenessReporter{
		Store:     beads.NewMemStore(),
		live:      true,
		lastFresh: now, // zero snapshot age
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	// Seed a warm body old enough to serve (< statusWarmServeMaxAge) but well
	// past the CLI banner threshold.
	const warmAge = 90 * time.Second
	srv.setWarmStatusBody(false, StatusBody{Name: "aged-warm-body"}, time.Now().Add(-warmAge))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/status"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	raw := rec.Header().Get("X-GC-Cache-Age-S")
	if raw == "" {
		t.Fatal("warm-served response missing X-GC-Cache-Age-S header")
	}
	age, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("X-GC-Cache-Age-S = %q, want a float: %v", raw, err)
	}
	if age < cacheAgeBannerThresholdSecondsForTest {
		t.Fatalf("X-GC-Cache-Age-S = %v for a %s-old warm body on a fresh store, want >= %v; "+
			"reporting only the store snapshot age hides the warm delay and suppresses the CLI staleness banner",
			age, warmAge, cacheAgeBannerThresholdSecondsForTest)
	}
}

// cacheAgeBannerThresholdSecondsForTest mirrors cmd/gc's
// cacheAgeBannerThresholdSeconds (30.0). internal/api cannot import cmd/gc, so
// the threshold this guard defends is restated here.
const cacheAgeBannerThresholdSecondsForTest = 30.0

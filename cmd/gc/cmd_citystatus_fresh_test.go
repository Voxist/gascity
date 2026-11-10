package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// setAlphaSuspended writes rig alpha's runtime suspension state as gc rig
// suspend and gc rig resume do.
func setAlphaSuspended(t *testing.T, h *charHarness, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, h.cityPath, "alpha", &suspended); err != nil {
		t.Fatalf("set rig alpha suspended=%v: %v", suspended, err)
	}
}

// alphaSuspendedIn decodes `gc status --json` and reports rig alpha's
// suspended flag.
func alphaSuspendedIn(t *testing.T, raw []byte) bool {
	t.Helper()
	var out StatusJSON
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode gc status --json: %v\n%s", err, raw)
	}
	for _, rig := range out.Rigs {
		if rig.Name == "alpha" {
			return rig.Suspended
		}
	}
	t.Fatalf("gc status --json has no rig alpha:\n%s", raw)
	return false
}

// warmStatusConvergeWithin is the safety deadline for a change to reach gc
// status on this fork. The CLI reads the server's warm StatusView (vp-e0hv),
// which the server rebuilds in the background once it is older than its 5 s
// refresh age, so a read right after a change may still show the previous
// body and a later read shows the change. Upstream's ?fresh=true read, which
// builds the body for every read, is not what gc status sends here.
const warmStatusConvergeWithin = 30 * time.Second

// awaitWarmStatus is the one polling boundary for these tests (TESTING.md,
// "Asynchronous tests wait for facts"): the CLI's view of the supervisor is a
// black box that signals no rebuild, and adding a signal would change the
// status contract. It re-reads on a context-bounded ticker until read reports
// ok, and fails with the last observed state when the deadline passes first.
func awaitWarmStatus(t *testing.T, what string, read func() (ok bool, observed string)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), warmStatusConvergeWithin)
	defer cancel()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		ok, observed := read()
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: not observed within %s; last read: %s", what, warmStatusConvergeWithin, observed)
			return
		case <-tick.C:
		}
	}
}

// gc status shows a change made since the previous read once the server's
// warm StatusView has been rebuilt, and that read names no rebuild.
func TestCityStatusShowsAChangeMadeSinceThePreviousRead(t *testing.T) {
	h := newCityStatusRouteHarness(t)
	for _, lane := range apiLanes(t, h) {
		setAlphaSuspended(t, h, false)
		awaitWarmStatus(t, lane.name+": gc status shows alpha resumed", func() (bool, string) {
			out, _ := cityStatusVia(t, h, lane, true)
			return !alphaSuspendedIn(t, out), string(out)
		})

		setAlphaSuspended(t, h, true)
		awaitWarmStatus(t, lane.name+": gc status shows alpha suspended once the warm view is rebuilt", func() (bool, string) {
			out, _ := cityStatusVia(t, h, lane, true)
			return alphaSuspendedIn(t, out), string(out)
		})
		if _, stderr := cityStatusVia(t, h, lane, false); strings.Contains(string(stderr), cacheAgeFooterMark) {
			t.Errorf("%s: text-mode stderr has a cache-age footer on a recently rebuilt warm body:\n%s", lane.name, stderr)
		}
	}
}

// gc rig status shows an agent started since the previous read once the
// server's warm StatusView has been rebuilt.
func TestRigStatusShowsAnAgentStartedSinceThePreviousRead(t *testing.T) {
	h := newCityStatusRouteHarness(t)
	sp, ok := h.cs.SessionProvider().(*runtime.Fake)
	if !ok {
		t.Fatalf("controller session provider = %T, want *runtime.Fake", h.cs.SessionProvider())
	}
	sessionName := agent.SessionNameFor(charCityName, "alpha/rigbot", h.cs.Config().Workspace.SessionTemplate)
	rigbotRunning := func(t *testing.T, raw []byte) bool {
		t.Helper()
		row, found := rigStatusRowsByQualifiedName(t, decodeRigStatusPayload(t, raw))["alpha/rigbot"]
		if !found {
			t.Fatalf("gc rig status --json has no alpha/rigbot row:\n%s", raw)
		}
		return row.Running
	}
	for _, lane := range apiLanes(t, h) {
		_ = sp.Stop(sessionName)
		readRunning := func(want bool) func() (bool, string) {
			return func() (bool, string) {
				code, out, errOut := runRigStatusRoute(t, h.cityPath, sp, newFakeDrainOps(), "alpha", lane.client, "", true)
				if code != 0 {
					t.Fatalf("%s: gc rig status exit %d: %s", lane.name, code, errOut)
				}
				return rigbotRunning(t, out) == want, string(out)
			}
		}
		awaitWarmStatus(t, lane.name+": gc rig status shows rigbot stopped", readRunning(false))

		if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
			t.Fatalf("start rigbot: %v", err)
		}
		awaitWarmStatus(t, lane.name+": gc rig status shows rigbot running once the warm view is rebuilt", readRunning(true))
		if _, _, errOut := runRigStatusRoute(t, h.cityPath, sp, newFakeDrainOps(), "alpha", lane.client, "", false); bytes.Contains(errOut, []byte(cacheAgeFooterMark)) {
			t.Errorf("%s: text-mode stderr has a cache-age footer on a recently rebuilt warm body:\n%s", lane.name, errOut)
		}
	}
}

// freshIgnoringSupervisor is a supervisor that predates ?fresh: it records the
// query each /status read sends, ignores it, and answers with a saved body
// 300 s old. With rebuilding set it reports the refresh headers a #8756-era
// supervisor sends; without, only the age an older one sends.
type freshIgnoringSupervisor struct {
	mu      sync.Mutex
	queries []url.Values
}

func (s *freshIgnoringSupervisor) handler(rebuilding bool, body map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.Query())
		s.mu.Unlock()
		w.Header().Set("X-GC-Cache-Age-S", "300")
		if rebuilding {
			w.Header().Set("X-GC-Cache-Refresh", "rebuilding")
			w.Header().Set("X-GC-Cache-Refresh-Interval-S", "30")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}

// requireWarmRead fails unless every recorded read left ?fresh unset: the
// fork's CLI reads the warm StatusView (vp-e0hv) and reports the age the
// server sends with it.
func (s *freshIgnoringSupervisor) requireWarmRead(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queries) == 0 {
		t.Fatal("the CLI made no /status read")
	}
	for _, q := range s.queries {
		if q.Has("fresh") {
			t.Errorf("/status query = %v, want no fresh parameter", q)
		}
	}
}

// olderSupervisorFooters are the footers a supervisor that ignores ?fresh
// provokes: the rebuild it names, or the bare age an older one reports.
var olderSupervisorFooters = map[bool]string{
	true:  "(cache age: 300s — the server answered with a response it saved that long ago and is building a fresh one",
	false: "(cache age: 300s — this read is over 240s old, and the server does not report why",
}

// A supervisor that ignores ?fresh still serves gc status, and the CLI reports
// exactly the age and cause that supervisor sends, claiming no freshness it
// did not get.
func TestRouteCityStatus_ServerIgnoringFreshKeepsItsReport(t *testing.T) {
	cityPath := writeCityStatusTestCity(t)
	cfg, err := loadCityConfig(cityPath, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	body := map[string]any{
		"name": "test-city", "path": "/tmp/test-city",
		"uptime_sec": 1, "suspended": false,
		"agent_count": 0, "rig_count": 0, "running": 0,
		"agents": map[string]any{"total": 0},
		"rigs":   map[string]any{"total": 0},
		"work":   map[string]any{}, "mail": map[string]any{},
	}
	for _, rebuilding := range []bool{true, false} {
		t.Run(map[bool]string{true: "reports rebuilding", false: "reports age only"}[rebuilding], func(t *testing.T) {
			sup := &freshIgnoringSupervisor{}
			srv := httptest.NewServer(sup.handler(rebuilding, body))
			defer srv.Close()
			c := api.NewCityScopedClient(srv.URL, "test-city")

			for _, jsonOut := range []bool{false, true} {
				var stdout, stderr bytes.Buffer
				if code := routeCityStatus(cityPath, cfg, newFakeDrainOps(), c, "", jsonOut, &stdout, &stderr, func() int {
					t.Errorf("gc status (json=%v) fell back to the local route: %s", jsonOut, stderr.String())
					return 1
				}); code != 0 {
					t.Fatalf("gc status (json=%v) exit = %d; stderr=%s", jsonOut, code, stderr.String())
				}
				if strings.Contains(stdout.String(), cacheAgeFooterMark) {
					t.Errorf("gc status (json=%v) stdout carries a cache-age footer:\n%s", jsonOut, stdout.String())
				}
				if !jsonOut {
					if want := olderSupervisorFooters[rebuilding]; !strings.Contains(stderr.String(), want) {
						t.Errorf("gc status stderr = %q, want it to contain %q", stderr.String(), want)
					}
					continue
				}
				var out StatusJSON
				if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
					t.Fatalf("decode gc status --json: %v\n%s", err, stdout.String())
				}
				if out.CacheAgeS == nil || *out.CacheAgeS != 300 {
					t.Errorf("gc status --json _cache_age_s = %v, want 300, the age the supervisor reported", out.CacheAgeS)
				}
			}
			sup.requireWarmRead(t)
		})
	}
}

// A supervisor that ignores ?fresh still serves gc rig status, with the age
// and cause it reports.
func TestRouteRigStatus_ServerIgnoringFreshKeepsItsReport(t *testing.T) {
	cityPath := writeRigStatusTestCity(t)
	body := map[string]any{
		"name": "test-city", "path": "/tmp",
		"uptime_sec": 1, "suspended": false,
		"agent_count": 0, "rig_count": 0, "running": 0,
		"agents": map[string]any{}, "rigs": map[string]any{},
		"work": map[string]any{}, "mail": map[string]any{},
		"rig_details": []map[string]any{
			{"name": "frontend", "path": "/tmp/frontend", "suspended": false},
		},
	}
	for _, rebuilding := range []bool{true, false} {
		t.Run(map[bool]string{true: "reports rebuilding", false: "reports age only"}[rebuilding], func(t *testing.T) {
			sup := &freshIgnoringSupervisor{}
			srv := httptest.NewServer(sup.handler(rebuilding, body))
			defer srv.Close()
			c := api.NewCityScopedClient(srv.URL, "test-city")

			rig := config.Rig{Name: "frontend", Path: "/tmp/frontend"}
			var stdout, stderr bytes.Buffer
			if code := routeRigStatus(cityPath, "test-city", rig, nil, "", nil, nil, nil, runtime.NewFake(), newFakeDrainOps(), c, "", false, &stdout, &stderr); code != 0 {
				t.Fatalf("gc rig status exit = %d; stderr=%s", code, stderr.String())
			}
			if strings.Contains(stdout.String(), cacheAgeFooterMark) {
				t.Errorf("gc rig status stdout carries a cache-age footer:\n%s", stdout.String())
			}
			if want := olderSupervisorFooters[rebuilding]; !strings.Contains(stderr.String(), want) {
				t.Errorf("gc rig status stderr = %q, want it to contain %q", stderr.String(), want)
			}
			sup.requireWarmRead(t)
		})
	}
}

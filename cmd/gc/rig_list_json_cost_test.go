package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Regression coverage for vp-zdp6e: `gc rig list --json` paid a fixed
// multi-second cost that the human listing did not. Measured on the fleet
// host at 2026-09-07, the JSON branch cost 3.0-3.6s against 0.3s plain, split
// between a session-provider construction that opens the bead store (~1.0s)
// and one session probe per configured rig (150-450ms each, O(rigs)) — spent
// on rigs that were suspended and therefore false by definition. On the API
// lane the same branch re-asked the supervisor whether the controller was
// running after a controller API read had already answered it.
//
// These tests pin the shape of the fix, not the timing: they count round
// trips and probes, so they fail on a reintroduced N+1 or a reintroduced
// supervisor dial without depending on how fast the host is.

// writeRigListCostCity writes a city.toml with the named rigs, suspending the
// ones named in suspended, and returns the city path.
func writeRigListCostCity(t *testing.T, rigs []string, suspended map[string]bool) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("[workspace]\nname = \"test-city\"\n")
	for _, name := range rigs {
		rigPath := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		// max_active_sessions = 1 keeps the agent single-session, so
		// rigHasRunningAgent probes one deterministic session name per rig
		// instead of going through pool discovery.
		fmt.Fprintf(&b, "\n[[agent]]\nname = %q\ndir = %q\nmax_active_sessions = 1\n", "worker-"+name, name)
		fmt.Fprintf(&b, "\n[[rigs]]\nname = %q\npath = %q\n", name, rigPath)
		if suspended[name] {
			b.WriteString("suspended_on_start = true\n")
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func decodeRigListJSON(t *testing.T, stdout *bytes.Buffer) RigListJSON {
	t.Helper()
	var out RigListJSON
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal rig list JSON: %v\n%s", err, stdout.String())
	}
	return out
}

// startAgentSessions makes every configured agent's session live in fake, using
// the same helpers the render uses so the names cannot drift apart.
func startAgentSessions(t *testing.T, cityPath string, fake *runtime.Fake) {
	t.Helper()
	cfg, err := loadCityConfigFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cityName := cfg.EffectiveCityName()
	for i := range cfg.Agents {
		name := sessionName(nil, cityName, cfg.Agents[i].QualifiedName(), cfg.Workspace.SessionTemplate)
		if err := fake.Start(t.Context(), name, runtime.Config{}); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
	}
	fake.Calls = nil
}

// TestDoRigListJSON_SuspendedRigsSkipSessionProbes: a suspended rig is false by
// definition (suspension state is the authority both lanes consult first), so
// the JSON branch must not spend a session probe asking. Every rig's agent
// session is deliberately LIVE here, so a render that still probes reports the
// suspended rigs as running and fails.
func TestDoRigListJSON_SuspendedRigsSkipSessionProbes(t *testing.T) {
	cityPath := writeRigListCostCity(t,
		[]string{"awake", "asleep-a", "asleep-b"},
		map[string]bool{"asleep-a": true, "asleep-b": true})

	fake := runtime.NewFake()
	startAgentSessions(t, cityPath, fake)
	orig := rigListSessionProvider
	rigListSessionProvider = func() (runtime.Provider, error) { return fake, nil }
	t.Cleanup(func() { rigListSessionProvider = orig })

	var stdout, stderr bytes.Buffer
	if code := doRigList(fsys.OSFS{}, cityPath, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doRigList = %d, stderr: %s", code, stderr.String())
	}

	// No probe may name a suspended rig's agent session.
	for _, call := range fake.Calls {
		for _, sleeper := range []string{"asleep-a", "asleep-b"} {
			if strings.Contains(call.Name, sleeper) {
				t.Errorf("suspended rig %s was probed: %s(%q)", sleeper, call.Method, call.Name)
			}
		}
	}

	out := decodeRigListJSON(t, &stdout)
	seen := 0
	for _, r := range out.Rigs {
		if r.HQ {
			continue
		}
		seen++
		wantSuspended := strings.HasPrefix(r.Name, "asleep-")
		if r.Suspended != wantSuspended {
			t.Errorf("rig %s suspended = %v, want %v", r.Name, r.Suspended, wantSuspended)
		}
		if wantSuspended && r.Running {
			t.Errorf("suspended rig %s reported running despite a live session", r.Name)
		}
		// The awake rig proves the probe still happens where it matters:
		// its session is live, so it must report running.
		if !wantSuspended && !r.Running {
			t.Errorf("awake rig %s reported not running despite a live session", r.Name)
		}
	}
	if seen != 3 {
		t.Fatalf("rigs rendered = %d, want 3", seen)
	}
}

// TestDoRigListJSON_AllSuspendedBuildsNoSessionProvider: when every rig is
// suspended there is nothing to probe, so the JSON branch must not construct
// the session provider at all. Construction alone opens the bead store and
// cost ~1.0s of the 3.5s on the fleet host.
func TestDoRigListJSON_AllSuspendedBuildsNoSessionProvider(t *testing.T) {
	cityPath := writeRigListCostCity(t,
		[]string{"asleep-a", "asleep-b", "asleep-c"},
		map[string]bool{"asleep-a": true, "asleep-b": true, "asleep-c": true})

	constructions := 0
	orig := rigListSessionProvider
	rigListSessionProvider = func() (runtime.Provider, error) {
		constructions++
		return runtime.NewFake(), nil
	}
	t.Cleanup(func() { rigListSessionProvider = orig })

	var stdout, stderr bytes.Buffer
	if code := doRigList(fsys.OSFS{}, cityPath, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doRigList = %d, stderr: %s", code, stderr.String())
	}
	if constructions != 0 {
		t.Fatalf("session provider constructed %d times for an all-suspended fleet, want 0", constructions)
	}
	if got := len(decodeRigListJSON(t, &stdout).Rigs); got != 4 {
		t.Fatalf("rigs in JSON = %d, want 4 (HQ + 3)", got)
	}
}

// TestRenderRigListFromAPI_JSONMakesNoSupervisorRoundTrip: the API lane render
// is reached only after a controller API read returned, which already proves
// the controller is running. Re-asking the supervisor for that fact is the
// fixed cost this bead removed, so the JSON branch must make no supervisor
// call and must still report HQ running.
func TestRenderRigListFromAPI_JSONMakesNoSupervisorRoundTrip(t *testing.T) {
	cityPath := writeRigListTestCity(t)

	supervisorCalls := 0
	origAlive := supervisorAliveHook
	supervisorAliveHook = func() int {
		supervisorCalls++
		return 0
	}
	t.Cleanup(func() { supervisorAliveHook = origAlive })

	origRunning := supervisorCityRunningHook
	supervisorCityRunningHook = func(string) (bool, string, bool) {
		supervisorCalls++
		return false, "", true
	}
	t.Cleanup(func() { supervisorCityRunningHook = origRunning })

	cr := api.CachedRead[[]api.RigView]{
		Body: []api.RigView{{Name: "frontend", Path: "/abs/frontend", Prefix: "fe"}},
	}
	var stdout, stderr bytes.Buffer
	if code := renderRigListFromAPI(fsys.OSFS{}, cityPath, cr, true, &stdout, &stderr); code != 0 {
		t.Fatalf("renderRigListFromAPI = %d, stderr: %s", code, stderr.String())
	}

	if supervisorCalls != 0 {
		t.Errorf("JSON render made %d supervisor round trips, want 0", supervisorCalls)
	}

	out := decodeRigListJSON(t, &stdout)
	if len(out.Rigs) == 0 || !out.Rigs[0].HQ {
		t.Fatalf("first row is not HQ: %s", stdout.String())
	}
	// Both hooks are pinned to "supervisor dead"; HQ still reports running
	// because the API read that reached this render proves the controller is.
	if !out.Rigs[0].Running {
		t.Errorf("HQ running = false, want true (derived from the successful API read)")
	}
}

// TestRigListJSON_SchemaUnchanged: the cost fix must not move the wire format.
func TestRigListJSON_SchemaUnchanged(t *testing.T) {
	cityPath := writeRigListCostCity(t, []string{"awake", "asleep"}, map[string]bool{"asleep": true})

	orig := rigListSessionProvider
	rigListSessionProvider = func() (runtime.Provider, error) { return runtime.NewFake(), nil }
	t.Cleanup(func() { rigListSessionProvider = orig })

	var stdout, stderr bytes.Buffer
	if code := doRigList(fsys.OSFS{}, cityPath, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doRigList = %d, stderr: %s", code, stderr.String())
	}

	var raw map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	for _, key := range []string{"schema_version", "city_path", "city_name", "rigs", "summary"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("top-level key %q missing: %s", key, stdout.String())
		}
	}
	if raw["schema_version"] != "1" {
		t.Errorf("schema_version = %#v, want \"1\"", raw["schema_version"])
	}
	rigs, ok := raw["rigs"].([]any)
	if !ok || len(rigs) != 3 {
		t.Fatalf("rigs = %#v, want 3 entries", raw["rigs"])
	}
	first, ok := rigs[0].(map[string]any)
	if !ok {
		t.Fatalf("rig row is not an object: %#v", rigs[0])
	}
	for _, key := range []string{"name", "path", "prefix", "hq", "suspended", "running", "beads"} {
		if _, ok := first[key]; !ok {
			t.Errorf("rig row key %q missing: %#v", key, first)
		}
	}
	summary, ok := raw["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary is not an object: %#v", raw["summary"])
	}
	for _, key := range []string{"total", "suspended", "running"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("summary key %q missing: %#v", key, summary)
		}
	}
}

// corpseListingProvider models the real divergence between the two liveness
// reads a session provider offers. ListRunning reports what the runtime still
// has a session record for; IsRunning is the authority on whether that session
// is actually alive. On tmux they genuinely disagree: IsRunning goes through
// the state cache, which excludes remain-on-exit corpses (pane_dead=1) and
// fails safe to "not running" for every session once the cache passes its 30s
// stale TTL, while ListRunning reads list-sessions and includes corpses. The
// t3bridge provider disagrees too — it requires a running/ready status in
// IsRunning but not in ListRunning.
//
// So a rig-list render must never infer running from a listing. This fake
// makes the disagreement explicit: corpse is listed but not running.
type corpseListingProvider struct {
	runtime.Provider
	corpse string
}

func (p *corpseListingProvider) ListRunning(prefix string) ([]string, error) {
	names, err := p.Provider.ListRunning(prefix)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(p.corpse, prefix) {
		names = append(names, p.corpse)
	}
	return names, nil
}

// TestDoRigListJSON_ListedButNotRunningSessionIsNotRunning: a session the
// provider still lists but reports as not running must not make its rig
// running. Guards against reintroducing a listing-derived shortcut for the
// per-rig liveness answer, which would resurrect tmux corpses and every
// session at once whenever the state cache goes stale.
func TestDoRigListJSON_ListedButNotRunningSessionIsNotRunning(t *testing.T) {
	cityPath := writeRigListCostCity(t, []string{"rig-a"}, nil)

	cfg, err := loadCityConfigFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(cfg.Agents))
	}
	corpse := sessionName(nil, cfg.EffectiveCityName(), cfg.Agents[0].QualifiedName(), cfg.Workspace.SessionTemplate)

	// The fake is never told to Start the session, so IsRunning(corpse) is
	// false while the wrapper still lists it.
	fake := runtime.NewFake()
	provider := &corpseListingProvider{Provider: fake, corpse: corpse}
	if provider.IsRunning(corpse) {
		t.Fatalf("fixture is wrong: IsRunning(%q) must be false", corpse)
	}
	listed, err := provider.ListRunning("")
	if err != nil || !slices.Contains(listed, corpse) {
		t.Fatalf("fixture is wrong: ListRunning must include %q, got %v (%v)", corpse, listed, err)
	}

	orig := rigListSessionProvider
	rigListSessionProvider = func() (runtime.Provider, error) { return provider, nil }
	t.Cleanup(func() { rigListSessionProvider = orig })

	var stdout, stderr bytes.Buffer
	if code := doRigList(fsys.OSFS{}, cityPath, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doRigList = %d, stderr: %s", code, stderr.String())
	}
	for _, r := range decodeRigListJSON(t, &stdout).Rigs {
		if r.HQ {
			continue
		}
		if r.Running {
			t.Errorf("rig %s reported running from a listing entry whose IsRunning is false", r.Name)
		}
	}
}

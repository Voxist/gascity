package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// unresolvedProviderEnv is the ga-8a8fq shape: two rigs, each with a pool
// agent holding one live session. One rig's provider cannot be resolved (its
// binary is not on PATH), which config load never checks but the per-tick
// desired-state build does, agent by agent.
type unresolvedProviderEnv struct {
	cityPath string
	cfg      *config.City
	store    beads.Store
	sp       *runtime.Fake
	good     beads.Bead
	broken   beads.Bead
}

const (
	unresolvedGoodTemplate   = "rig-a/worker"
	unresolvedBrokenTemplate = "rig-b/worker"
)

func newUnresolvedProviderEnv(t *testing.T, brokenCommand string) *unresolvedProviderEnv {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"healthy":    {Command: "true"},
			"unresolved": {Command: brokenCommand},
		},
		Rigs: []config.Rig{
			{Name: "rig-a", Path: filepath.Join(cityPath, "rig-a")},
			{Name: "rig-b", Path: filepath.Join(cityPath, "rig-b")},
		},
		Agents: []config.Agent{
			{Name: "worker", Dir: "rig-a", Provider: "healthy", MaxActiveSessions: intPtr(1), ScaleCheck: "printf 1"},
			{Name: "worker", Dir: "rig-b", Provider: "unresolved", MaxActiveSessions: intPtr(1), ScaleCheck: "printf 1"},
		},
	}
	for _, rig := range cfg.Rigs {
		if err := os.MkdirAll(rig.Path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rig.Path, err)
		}
	}
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mk := func(template, sessionName string) beads.Bead {
		b, err := store.Create(beads.Bead{
			Title:  template,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "agent:" + template, "template:" + template},
			Metadata: map[string]string{
				"template":             template,
				"agent_name":           template,
				"alias":                template,
				"session_name":         sessionName,
				"state":                "awake",
				"generation":           "1",
				"continuation_epoch":   "1",
				poolManagedMetadataKey: boolMetadata(true),
			},
		})
		if err != nil {
			t.Fatalf("create session bead %s: %v", template, err)
		}
		if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
			t.Fatalf("start %s: %v", sessionName, err)
		}
		return b
	}
	env := &unresolvedProviderEnv{cityPath: cityPath, cfg: cfg, store: store, sp: sp}
	env.good = mk(unresolvedGoodTemplate, "s-rig-a-worker")
	env.broken = mk(unresolvedBrokenTemplate, "s-rig-b-worker")
	return env
}

// tick runs the controller's own composition: the real desired-state build,
// then the bead-reconcile tick over its result.
func (e *unresolvedProviderEnv) tick(t *testing.T) (*CityRuntime, DesiredStateResult, string) {
	t.Helper()
	var stderr bytes.Buffer
	snapshot, err := loadSessionBeadSnapshot(e.store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	now := time.Now().UTC()
	result := buildDesiredStateWithSessionBeadsAt("test-city", e.cityPath, now, now, e.cfg, e.sp, e.store, nil, snapshot, nil, &stderr)
	cr := &CityRuntime{
		cityPath:            e.cityPath,
		cityName:            "test-city",
		cfg:                 e.cfg,
		sp:                  e.sp,
		standaloneCityStore: e.store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.NewFake(),
		logPrefix:           "gc",
		stdout:              io.Discard,
		stderr:              &stderr,
	}
	snapshot, err = loadSessionBeadSnapshot(e.store)
	if err != nil {
		t.Fatalf("reload session snapshot: %v", err)
	}
	cr.beadReconcileTick(context.Background(), result, snapshot, nil, false)
	return cr, result, stderr.String()
}

// TestUnresolvableProviderDoesNotDrainItsSessionsAsOrphaned is the ga-8a8fq
// regression. An agent whose provider cannot be resolved is left out of the
// desired set; before the fix its live pool session then read as "not
// controller-owned" and was drained as orphaned. Because the provider catalog
// is city-wide, a provider shared by every rig took every session with it.
//
// The failure must stay with the failing agent: its session is kept (fail
// closed), the resolution error is reported, and the healthy rig reconciles
// normally.
func TestUnresolvableProviderDoesNotDrainItsSessionsAsOrphaned(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "gc-ga-8a8fq-no-such-provider-binary")

	cr, result, stderr := env.tick(t)

	if _, ok := result.State["s-rig-a-worker"]; !ok {
		t.Fatalf("healthy rig session missing from desired state %v; stderr:\n%s", keysOf(result.State), stderr)
	}
	if ds := cr.sessionDrains.get(env.broken.ID); ds != nil {
		t.Fatalf("session of the agent whose provider could not be resolved is draining (reason %q): an unresolvable provider must fail closed for that agent, not orphan its sessions; stderr:\n%s", ds.reason, stderr)
	}
	if ds := cr.sessionDrains.get(env.good.ID); ds != nil {
		t.Fatalf("healthy rig session is draining (reason %q); stderr:\n%s", ds.reason, stderr)
	}
	if !env.sp.IsRunning("s-rig-b-worker") {
		t.Fatal("session of the unresolvable agent was stopped")
	}
	if !strings.Contains(stderr, `keeping existing sessions of "`+unresolvedBrokenTemplate+`"`) ||
		!strings.Contains(stderr, "gc-ga-8a8fq-no-such-provider-binary") {
		t.Fatalf("resolution failure not reported with the kept template and its cause; stderr:\n%s", stderr)
	}
}

// TestSharedUnresolvableProviderKeepsEverySession is the observed blast radius:
// every rig's agent names the same provider, so one unresolvable catalog entry
// fails resolution for all of them. None of their sessions may be drained.
func TestSharedUnresolvableProviderKeepsEverySession(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "gc-ga-8a8fq-no-such-provider-binary")
	env.cfg.Agents[0].Provider = "unresolved"

	cr, _, stderr := env.tick(t)

	for _, b := range []beads.Bead{env.good, env.broken} {
		if ds := cr.sessionDrains.get(b.ID); ds != nil {
			t.Fatalf("session %s is draining (reason %q) because a shared provider could not be resolved; stderr:\n%s", b.Metadata["session_name"], ds.reason, stderr)
		}
	}
}

// TestRemovedAgentSessionStillDrainsAsOrphaned is the control: the fail-closed
// guard is scoped to agents that are still configured but unresolvable. A
// session whose agent was removed from config keeps draining as orphaned.
func TestRemovedAgentSessionStillDrainsAsOrphaned(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "true")
	env.cfg.Agents = env.cfg.Agents[:1]

	cr, _, stderr := env.tick(t)

	ds := cr.sessionDrains.get(env.broken.ID)
	if ds == nil || ds.reason != "orphaned" {
		t.Fatalf("removed agent's session drain = %+v, want an orphaned drain; stderr:\n%s", ds, stderr)
	}
}

func keysOf(m map[string]TemplateParams) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestProviderAbsentFromCatalogKeepsItsSessions covers the portharbour shape as
// it reaches the build: an agent whose provider NAME is not in the city's
// [providers.*] catalog at all (a pack default that was never patched), as
// opposed to a cataloged provider whose binary is missing. Its sessions must
// be kept, and an attached session must never be drained on this path.
func TestProviderAbsentFromCatalogKeepsItsSessions(t *testing.T) {
	env := newUnresolvedProviderEnv(t, "true")
	env.cfg.Agents[1].Provider = "zai"
	env.sp.SetAttached("s-rig-b-worker", true)

	cr, result, stderr := env.tick(t)

	if _, ok := result.UnresolvedTemplates[unresolvedBrokenTemplate]; !ok {
		t.Fatalf("UnresolvedTemplates = %v, want %q recorded; stderr:\n%s", result.UnresolvedTemplates, unresolvedBrokenTemplate, stderr)
	}
	if ds := cr.sessionDrains.get(env.broken.ID); ds != nil {
		t.Fatalf("attached session of an agent whose provider is absent from the catalog is draining (reason %q); stderr:\n%s", ds.reason, stderr)
	}
	if !env.sp.IsRunning("s-rig-b-worker") {
		t.Fatal("attached session of the uncataloged-provider agent was stopped")
	}
	if ds := cr.sessionDrains.get(env.good.ID); ds != nil {
		t.Fatalf("healthy rig session is draining (reason %q); stderr:\n%s", ds.reason, stderr)
	}
}

// TestReloadWithUncataloguedPackDefaultProviderKeepsLastGoodConfig pins the
// load-time half of the same failure. A pack whose agent defaults to a provider
// the city does not declare fails config composition as a whole, city-wide.
// That error must never become "desired = empty": the controller keeps the
// last good config, so the next build still wants every session it wanted.
func TestReloadWithUncataloguedPackDefaultProviderKeepsLastGoodConfig(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: &stderr,
	})
	oldCfg := cr.cfg

	packDir := filepath.Join(cityPath, "packs", "fleet")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(`[pack]
name = "fleet"
schema = 2

[[agent]]
name = "executor"
provider = "zai"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tomlPath, []byte(`[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "fake"

[imports.fleet]
source = "packs/fleet"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q (error %q)", reply.Outcome, reloadOutcomeFailed, reply.Error)
	}
	if !strings.Contains(reply.Error, `"zai"`) {
		t.Fatalf("reply.Error = %q, want the uncataloged provider named", reply.Error)
	}
	if cr.cfg != oldCfg {
		t.Fatal("config replaced by one that failed provider-catalog validation; the last good config must stay in force")
	}
}

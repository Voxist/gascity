package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// AC6 of the vc-ny00 store-warming plan, and the plan's constraint 1:
// read_timeout_millis stays at the operator-settled steady-state value, and
// nothing this plan adds writes any listener deadline at all.
//
// WHY THIS TEST EXISTS RATHER THAN A COMMENT. The settled wall has been
// raised and reverted four times (city.toml's own comment history), and the
// 2026-09-05 incident (vp-9ex9z) was a nested start leaking its RAISED
// deadline into the SHARED published config — read_timeout_millis 600000
// serving live swarm traffic while city.toml said 30000. The failure mode is
// not "someone argues for a different number"; it is a code path that writes
// a deadline as a side effect and no one notices. So the invariant asserted
// here is structural: the store-warming layers contain no listener-deadline
// writer, and the only raised value in the tree remains the window server's.
//
// The plan's phrasing is "30000 (or the operator-set value)". 30000 is a
// DEPLOYMENT value living in the Voxist city.toml, not a constant in this
// repo — this repo's managed default is config.DefaultDoltReadTimeoutMillis.
// The repo-side invariant that carries the same guarantee is the one below:
// steady-state rendering resolves through EffectiveReadTimeoutMillis (which
// returns exactly what the operator set), and no vc-ny00 code path
// substitutes anything for it.

// storeWarmingPlanFiles are the non-test files this plan adds or owns. Any
// listener-deadline write appearing in one of them is the vp-9ex9z shape
// reappearing in a new layer.
var storeWarmingPlanFiles = []string{
	"store_warming_state.go",
	"store_warming_sweep.go",
	"store_warming_pass.go",
}

func TestStoreWarmingLayersWriteNoListenerDeadline(t *testing.T) {
	t.Parallel()

	// Matches an ASSIGNMENT to the config field or an emission of the YAML
	// key — the two ways a deadline actually reaches a server. A bare
	// mention in a comment (this plan reasons about the wall constantly) is
	// deliberately not a violation.
	writer := regexp.MustCompile(`ReadTimeoutMillis\s*[:=]|read_timeout_millis\s*:\s*%|read_timeout_millis\s*:\s*\d`)

	for _, name := range storeWarmingPlanFiles {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v — if this file was renamed, update "+
				"storeWarmingPlanFiles so the invariant keeps covering it", name, err)
		}
		if loc := writer.FindIndex(raw); loc != nil {
			line := 1 + strings.Count(string(raw[:loc[0]]), "\n")
			t.Errorf("%s:%d writes a listener read timeout; the vc-ny00 layers must "+
				"never write one (plan constraint 1). The steady-state wall is the "+
				"operator's, resolved through EffectiveReadTimeoutMillis; the only "+
				"raised value in the tree is the delivery window's.", name, line)
		}
	}
}

// TestListenerDeadlineWritersAreAnEnumeratedSet pins the "only raised
// value" half of AC6 across the whole command package, so a future layer
// cannot quietly become a second raiser.
//
// Two assignment sites are legitimate and each is legitimate for a DIFFERENT
// reason, which is why this is an allowlist rather than a count:
//
//   - dolt_delivery_window.go raises the deadline to 600000 for the NESTED
//     window server only. That is safe solely because the window is quiesced
//     by construction (publish=false, its own config file), and it is the one
//     raised value AC6 permits.
//   - dolt_start_managed.go resolves the GC_DOLT_READ_TIMEOUT_MILLIS env
//     override into the config when city.toml sets nothing. It is an
//     operator pass-through, not a raise — the same class as
//     EffectiveReadTimeoutMillis, and it predates this plan.
//
// A THIRD site is the thing to catch. vp-9ex9z was exactly a second writer
// reaching the config that serves the swarm.
func TestListenerDeadlineWritersAreAnEnumeratedSet(t *testing.T) {
	t.Parallel()

	allowed := map[string]string{
		"dolt_delivery_window.go": "the nested, quiesced window server's raised deadline (the one raise AC6 permits)",
		"dolt_start_managed.go":   "GC_DOLT_READ_TIMEOUT_MILLIS operator pass-through, not a raise",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	assign := regexp.MustCompile(`\.ReadTimeoutMillis\s*=`)

	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range assign.FindAllStringIndex(string(raw), -1) {
			if _, ok := allowed[name]; !ok {
				line := 1 + strings.Count(string(raw[:m[0]]), "\n")
				t.Errorf("%s:%d writes the listener read timeout, and is not one of "+
					"the two sanctioned writers. Every steady-state path must carry "+
					"the operator's value unchanged; a new writer is how the "+
					"2026-09-05 leak happened (vp-9ex9z).", name, line)
			}
			seen[name] = true
		}
	}

	for name, why := range allowed {
		if !seen[name] {
			t.Errorf("expected %s to still write the listener read timeout (%s); "+
				"it no longer does, so this allowlist is stale and is no longer "+
				"protecting what it claims to", name, why)
		}
	}
}

// TestDeliveryWindowRaisedValueIsUnchanged pins the one number this plan is
// allowed to leave raised, so "the only raised value remains 600000" is a
// checked statement rather than a claim in a comment.
func TestDeliveryWindowRaisedValueIsUnchanged(t *testing.T) {
	t.Parallel()

	if defaultDeliveryWindowReadTimeoutMillis != 600000 {
		t.Fatalf("delivery window read timeout = %d, want 600000 (AC6: the ONLY "+
			"raised value in the tree)", defaultDeliveryWindowReadTimeoutMillis)
	}
	if got := deliveryWindowReadTimeoutMillis(); got != 600000 {
		t.Fatalf("resolved delivery window read timeout = %d, want 600000", got)
	}
}

// TestSteadyStateRenderingUsesTheOperatorsValue pins the behavioral half:
// the config the swarm-facing server binds carries exactly what the operator
// set, with nothing from this plan substituted for it.
func TestSteadyStateRenderingUsesTheOperatorsValue(t *testing.T) {
	t.Parallel()

	// 30000 is the value the Voxist city settles on (operator decision B,
	// vc-t6xp/ADR-0064); the assertion is that rendering is a pass-through,
	// so any operator value survives.
	for _, want := range []int{30000, 45000, config.DefaultDoltReadTimeoutMillis} {
		dir := t.TempDir()
		path := filepath.Join(dir, "dolt-config.yaml")
		cfg := config.DoltConfig{}
		if want != config.DefaultDoltReadTimeoutMillis {
			cfg.ReadTimeoutMillis = want
		}
		if err := writeManagedDoltConfigFile(path, "127.0.0.1", "3306", dir, "warning", cfg); err != nil {
			t.Fatalf("render managed config: %v", err)
		}
		got, ok := readDoltConfigReadTimeoutMillis(path)
		if !ok {
			t.Fatalf("rendered config carries no read_timeout_millis")
		}
		if got != want {
			t.Fatalf("rendered read_timeout_millis = %d, want the operator's %d — "+
				"steady-state rendering must be a pass-through", got, want)
		}
	}
}

package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestRigFieldSync verifies that every overridable Rig field has a matching
// RigPatch field. When a new field is added to Rig it must also be added to
// RigPatch (or explicitly excluded below with a justification). Parallel to
// TestAgentFieldSync; before this test existed, AGENTS.md "Adding rig config
// fields" was the only thing standing between a new Rig field and a silent
// hole in the patch layer, and dolt_host/dolt_port fell through it.
//
// See AGENTS.md "Adding rig config fields" for the convention.
func TestRigFieldSync(t *testing.T) {
	// Fields that exist on Rig but are NOT overridable via a rig patch.
	// Add to this list with a comment explaining why.
	excluded := map[string]string{
		"Name": "identity field; RigPatch.Name is the targeting key, not an override",
		// Pack composition inputs. LoadWithIncludesOptions resolves rig
		// includes/imports and expands their agents before ApplyPatches runs
		// (internal/config/compose.go), so a patched value is read by nobody.
		// Layer packs by adding them to the fragment that declares the rig.
		"Includes": "V1 pack sources, resolved during composition before ApplyPatches runs",
		"Imports":  "V2 pack imports, resolved during composition before ApplyPatches runs",
		// Rig-level agent overrides are consumed by pack expansion, which also
		// precedes ApplyPatches. Post-merge agent changes belong in
		// [[patches.agent]], which applyAgentPatch handles.
		"Overrides":  "rig-scoped agent overrides, consumed by pack expansion before ApplyPatches; use [[patches.agent]]",
		"RigPatches": "V2 spelling of Overrides, same composition ordering",
	}

	rigFields := structFields(reflect.TypeOf(Rig{}))
	patchFields := structFields(reflect.TypeOf(RigPatch{}))

	var expected []string
	for _, f := range rigFields {
		if _, ok := excluded[f]; !ok {
			expected = append(expected, f)
		}
	}
	sort.Strings(expected)

	patchSet := toSet(patchFields)
	var missing []string
	for _, f := range expected {
		if !patchSet[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		t.Errorf("RigPatch missing fields that exist on Rig: %v\n"+
			"Add them to RigPatch and wire them into applyRigPatch, or add them to "+
			"the excluded map with justification.", missing)
	}

	// Catch the reverse drift: a RigPatch field with no Rig counterpart.
	// "Name" is the targeting key and also exists on Rig, so it needs no
	// patch-only exemption.
	rigSet := toSet(rigFields)
	for _, f := range patchFields {
		if !rigSet[f] {
			t.Errorf("RigPatch has field %q not found on Rig", f)
		}
	}
}

// TestApplyRigPatchCoversAllFields verifies that applyRigPatch actually merges
// every field on RigPatch. A field added to RigPatch but never wired into
// applyRigPatch parses from city.toml and is then silently dropped; this test
// fails instead. Same approach as TestApplyAgentPatchCoversAllFields: populate
// every patch field, apply to a zero-valued rig, assert nothing was left zero.
func TestApplyRigPatchCoversAllFields(t *testing.T) {
	strVal := func(s string) *string { return &s }
	intVal := func(n int) *int { return &n }
	boolVal := func(b bool) *bool { return &b }
	sliceVal := func(ss ...string) *[]string { return &ss }
	portVal := func(s string) *PortString { p := PortString(s); return &p }

	patch := RigPatch{
		Name:                "target-rig",
		Path:                strVal("/rigs/target"),
		Prefix:              strVal("tr"),
		DefaultBranch:       strVal("develop"),
		Suspended:           boolVal(true),
		SuspendedOnStart:    boolVal(true),
		FormulasDir:         strVal("formulas/target"),
		MaxActiveSessions:   intVal(7),
		DefaultSlingTarget:  strVal("target/polecat"),
		DefaultSlingTargets: sliceVal("target/polecat-a", "target/polecat-b"),
		SessionSleep: &SessionSleepConfig{
			InteractiveResume: "60s",
			InteractiveFresh:  "90s",
			NonInteractive:    "off",
		},
		DoltHost:    strVal("dolt.example.com"),
		DoltPort:    portVal("4406"),
		FormulaVars: map[string]string{"stakes": "2"},
	}

	// Every RigPatch field must be exercised by the test data, so a new field
	// cannot be added without also being covered here.
	pv := reflect.ValueOf(patch)
	pt := pv.Type()
	for i := 0; i < pt.NumField(); i++ {
		if pv.Field(i).IsZero() {
			t.Errorf("RigPatch field %q is zero in test data — add it to the test patch", pt.Field(i).Name)
		}
	}

	cfg := &City{Rigs: []Rig{{Name: "target-rig"}}}
	if err := applyRigPatch(cfg, &patch); err != nil {
		t.Fatalf("applyRigPatch: %v", err)
	}
	rig := cfg.Rigs[0]

	// "Name" is the targeting key: it selects the rig, it is not applied.
	targeting := map[string]bool{"Name": true}

	rv := reflect.ValueOf(rig)
	rt := rv.Type()
	rigFieldByName := make(map[string]int, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		rigFieldByName[rt.Field(i).Name] = i
	}

	for i := 0; i < pt.NumField(); i++ {
		fname := pt.Field(i).Name
		if targeting[fname] {
			continue
		}
		idx, ok := rigFieldByName[fname]
		if !ok {
			continue
		}
		if rv.Field(idx).IsZero() {
			t.Errorf("applyRigPatch did not apply field %q to Rig", fname)
		}
	}

	// Spot-check the non-scalar merges the zero check cannot distinguish from
	// a partial copy.
	if len(rig.DefaultSlingTargets) != 2 || rig.DefaultSlingTargets[1] != "target/polecat-b" {
		t.Errorf("DefaultSlingTargets = %v, want both entries", rig.DefaultSlingTargets)
	}
	if rig.SessionSleep.InteractiveResume != "60s" ||
		rig.SessionSleep.InteractiveFresh != "90s" ||
		rig.SessionSleep.NonInteractive != "off" {
		t.Errorf("SessionSleep = %+v, want all three class values applied", rig.SessionSleep)
	}
	if rig.FormulaVars["stakes"] != "2" {
		t.Errorf("FormulaVars[stakes] = %q, want %q", rig.FormulaVars["stakes"], "2")
	}
	if *rig.MaxActiveSessions != 7 {
		t.Errorf("MaxActiveSessions = %d, want 7", *rig.MaxActiveSessions)
	}
}

// TestRigPatchSessionSleepMergesPerClass verifies that a SessionSleep patch
// merges class by class rather than replacing the rig's whole block: a patch
// that sets only noninteractive must leave the rig's interactive values alone.
func TestRigPatchSessionSleepMergesPerClass(t *testing.T) {
	cfg := &City{Rigs: []Rig{{
		Name: "r",
		SessionSleep: SessionSleepConfig{
			InteractiveResume: "30s",
			InteractiveFresh:  "45s",
		},
	}}}
	patch := RigPatch{Name: "r", SessionSleep: &SessionSleepConfig{NonInteractive: "off"}}
	if err := applyRigPatch(cfg, &patch); err != nil {
		t.Fatalf("applyRigPatch: %v", err)
	}
	got := cfg.Rigs[0].SessionSleep
	want := SessionSleepConfig{InteractiveResume: "30s", InteractiveFresh: "45s", NonInteractive: "off"}
	if got != want {
		t.Errorf("SessionSleep = %+v, want %+v", got, want)
	}
}

// TestRigPatchDefaultSlingTargetsClears verifies the pointer-to-slice clear
// signal: an empty (non-nil) list removes the rig's targets, while a nil
// pointer leaves them untouched. Without the distinction a patch could add
// targets but never take the last one away.
func TestRigPatchDefaultSlingTargetsClears(t *testing.T) {
	cfg := &City{Rigs: []Rig{{Name: "r", DefaultSlingTargets: []string{"r/a"}}}}
	empty := []string{}
	if err := applyRigPatch(cfg, &RigPatch{Name: "r", DefaultSlingTargets: &empty}); err != nil {
		t.Fatalf("applyRigPatch: %v", err)
	}
	if len(cfg.Rigs[0].DefaultSlingTargets) != 0 {
		t.Errorf("DefaultSlingTargets = %v, want cleared", cfg.Rigs[0].DefaultSlingTargets)
	}

	cfg = &City{Rigs: []Rig{{Name: "r", DefaultSlingTargets: []string{"r/a"}}}}
	if err := applyRigPatch(cfg, &RigPatch{Name: "r"}); err != nil {
		t.Fatalf("applyRigPatch: %v", err)
	}
	if len(cfg.Rigs[0].DefaultSlingTargets) != 1 {
		t.Errorf("DefaultSlingTargets = %v, want untouched by a nil patch field", cfg.Rigs[0].DefaultSlingTargets)
	}
}

// TestRigDoltPortAcceptsBareInteger pins the decode contract for dolt_port.
// It is written as a port number, so the natural TOML spelling is the bare
// integer; a decoder that only accepts the quoted form turns one unquoted
// digit into a whole-file Parse failure and the city does not load at all.
// Both spellings must decode to the same string.
func TestRigDoltPortAcceptsBareInteger(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
	}{
		{"bare integer", "dolt_port = 9876"},
		{"quoted string", `dolt_port = "9876"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rig Rig
			if _, err := toml.Decode(tc.toml, &rig); err != nil {
				t.Fatalf("decoding %s: %v", tc.toml, err)
			}
			if got := string(rig.DoltPort); got != "9876" {
				t.Errorf("DoltPort = %q, want %q", got, "9876")
			}
		})
	}
}

// TestCityParseAcceptsBareRigDoltPort is the end-to-end form of the same
// contract: a city.toml with an unquoted dolt_port must load, because
// Parse turns any single decode error into a whole-config failure.
func TestCityParseAcceptsBareRigDoltPort(t *testing.T) {
	const cityTOML = `
name = "test-city"

[[rigs]]
name = "backend"
path = "rigs/backend"
dolt_host = "127.0.0.1"
dolt_port = 9876
`
	cfg, err := Parse([]byte(cityTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("parsed %d rigs, want 1", len(cfg.Rigs))
	}
	if got := string(cfg.Rigs[0].DoltPort); got != "9876" {
		t.Errorf("DoltPort = %q, want %q", got, "9876")
	}
}

// TestRigDoltPortRejectsNonNumeric keeps the looser decoder from becoming a
// silent sink: a value that is not a port number must still fail at parse,
// naming the offending value, rather than reaching the Dolt dialer.
func TestRigDoltPortRejectsNonNumeric(t *testing.T) {
	var rig Rig
	_, err := toml.Decode(`dolt_port = "not-a-port"`, &rig)
	if err == nil {
		t.Fatal("decoding a non-numeric dolt_port succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "not-a-port") {
		t.Errorf("error = %q, want it to name the rejected value", err)
	}
}

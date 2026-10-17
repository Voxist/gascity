package beadmeta

import "testing"

// TestIsParkedMetadataMirrorsThePackPredicate pins the binary-side park
// predicate to the same three signals the city pack's gclib.is_parked honors
// (gc.do_not_auto_route truthy, gc.awaiting_human non-empty, gc.blocked_on
// non-empty). The binary cannot import the pack's gclib, so this declaration
// is the contract: a park signal any pack writer stamps must read as parked
// here, or the route-recovery lanes will "repair" a deliberate discharge.
func TestIsParkedMetadataMirrorsThePackPredicate(t *testing.T) {
	cases := []struct {
		name string
		md   map[string]string
		want bool
	}{
		{
			name: "empty metadata is not parked",
			md:   map[string]string{},
			want: false,
		},
		{
			name: "unrelated metadata is not parked",
			md:   map[string]string{RunTargetMetadataKey: "gascity/gastown.polecat"},
			want: false,
		},
		{
			name: "awaiting_human parks",
			md:   map[string]string{AwaitingHumanMetadataKey: "karel@voxist.com"},
			want: true,
		},
		{
			name: "awaiting_human whitespace-only does not park",
			md:   map[string]string{AwaitingHumanMetadataKey: "   "},
			want: false,
		},
		{
			name: "blocked_on parks",
			md:   map[string]string{BlockedOnMetadataKey: "human-operator-oauth-credential"},
			want: true,
		},
		{
			name: "do_not_auto_route=1 parks",
			md:   map[string]string{DoNotAutoRouteMetadataKey: "1"},
			want: true,
		},
		{
			name: "do_not_auto_route=true parks",
			md:   map[string]string{DoNotAutoRouteMetadataKey: "true"},
			want: true,
		},
		{
			name: "do_not_auto_route=YES parks case-insensitively",
			md:   map[string]string{DoNotAutoRouteMetadataKey: "YES"},
			want: true,
		},
		{
			name: "do_not_auto_route blank does not park",
			md:   map[string]string{DoNotAutoRouteMetadataKey: ""},
			want: false,
		},
		{
			name: "do_not_auto_route=0 does not park",
			md:   map[string]string{DoNotAutoRouteMetadataKey: "0"},
			want: false,
		},
		{
			name: "do_not_auto_route prose does not park",
			md:   map[string]string{DoNotAutoRouteMetadataKey: "held for review"},
			want: false,
		},
		{
			name: "any one signal parks among unrelated keys",
			md: map[string]string{
				RunTargetMetadataKey:     "gascity/gastown.polecat",
				AwaitingHumanMetadataKey: "ops@voxist.com",
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsParkedMetadata(tc.md); got != tc.want {
				t.Fatalf("IsParkedMetadata(%#v) = %v, want %v", tc.md, got, tc.want)
			}
		})
	}
}

// TestIsOptOutValueMirrorsThePackNormalisation pins the opt-out value
// normalisation: bd stores `--set-metadata gc.do_not_auto_route=1` as the JSON
// integer 1 and `=true` as the JSON boolean true, and the beads StringMap
// decode coerces both to their string form — so the binary-side check sees
// "1"/"true" and must accept every spelling gclib.is_optout accepts.
func TestIsOptOutValueMirrorsThePackNormalisation(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "True", "yes", "Yes", " 1 ", " true\t"} {
		if !IsOptOutValue(v) {
			t.Errorf("IsOptOutValue(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "on", "held", "1.0"} {
		if IsOptOutValue(v) {
			t.Errorf("IsOptOutValue(%q) = true, want false", v)
		}
	}
}

// TestParkKeysPinWireValues guards the three park keys' exact strings: they
// are written by bd commands and pack scripts outside this repo and read by
// the route-restore paths inside it, so a rename here without a data migration
// would silently un-park every parked bead in the fleet.
func TestParkKeysPinWireValues(t *testing.T) {
	if AwaitingHumanMetadataKey != "gc.awaiting_human" {
		t.Fatalf("AwaitingHumanMetadataKey = %q, want %q", AwaitingHumanMetadataKey, "gc.awaiting_human")
	}
	if DoNotAutoRouteMetadataKey != "gc.do_not_auto_route" {
		t.Fatalf("DoNotAutoRouteMetadataKey = %q, want %q", DoNotAutoRouteMetadataKey, "gc.do_not_auto_route")
	}
	if BlockedOnMetadataKey != "gc.blocked_on" {
		t.Fatalf("BlockedOnMetadataKey = %q, want %q", BlockedOnMetadataKey, "gc.blocked_on")
	}
}

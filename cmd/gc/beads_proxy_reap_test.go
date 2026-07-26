package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestProxiedScopeBeadsDirs(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Path: "/srv/rig-a"},
			{Path: "/srv/rig-b"},
			{Path: ""}, // skipped
		},
	}
	got := proxiedScopeBeadsDirs("/srv/city", cfg)
	want := []string{
		filepath.Join("/srv/city", ".beads"),
		filepath.Join("/srv/rig-a", ".beads"),
		filepath.Join("/srv/rig-b", ".beads"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d dirs %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dir[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestProxyChildRootArg(t *testing.T) {
	cases := map[string]string{
		"bd db-proxy-child --root /a/.beads/proxieddb --port 5000":  "/a/.beads/proxieddb",
		"bd db-proxy-child --root=/b/.beads/proxieddb --backend ex": "/b/.beads/proxieddb",
		"bd db-proxy-child --port 5000 --backend external":          "",
		"some other process": "",
	}
	for cmd, want := range cases {
		if got := proxyChildRootArg(cmd); got != want {
			t.Errorf("proxyChildRootArg(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestProxyChildPIDsFromPS(t *testing.T) {
	beadsDirs := []string{
		"/srv/city/.beads",
		"/srv/rig-a/.beads",
	}
	ps := strings.Join([]string{
		"  101 bd db-proxy-child --root /srv/city/.beads/proxieddb --port 50001 --backend external",
		"  202 bd db-proxy-child --root /srv/rig-a/.beads/proxieddb --port 50002 --backend external",
		"  303 bd db-proxy-child --root /srv/rig-OTHER/.beads/proxieddb --port 50003", // not our scope
		"  404 dolt sql-server -H 127.0.0.1 -P 48770",                                 // not a proxy
		"  505 /usr/bin/bd list", // not a proxy child
		"garbage line without pid",
	}, "\n")

	pids := proxyChildPIDsFromPS(ps, beadsDirs)
	want := []int{101, 202}
	if len(pids) != len(want) {
		t.Fatalf("got %v, want %v", pids, want)
	}
	for i := range want {
		if pids[i] != want[i] {
			t.Fatalf("pid[%d] = %d, want %d", i, pids[i], want[i])
		}
	}
}

// TestReapProxiedChildrenForCity_NoopWhenNotProxied verifies reaping is inert
// (and never shells out to ps) when proxied mode is off.
func TestReapProxiedChildrenForCity_NoopWhenNotProxied(t *testing.T) {
	cfg := &config.City{} // Beads.Proxied unset -> disabled
	if n := reapProxiedChildrenForCity("/srv/city", cfg, nil); n != 0 {
		t.Fatalf("expected 0 reaped when proxied disabled, got %d", n)
	}
	if n := reapProxiedChildrenForCity("/srv/city", nil, nil); n != 0 {
		t.Fatalf("expected 0 reaped for nil cfg, got %d", n)
	}
}

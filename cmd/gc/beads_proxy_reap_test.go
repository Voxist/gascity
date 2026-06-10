package main

import (
	"os"
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

func TestProxiedReapRoots(t *testing.T) {
	boolp := func(v bool) *bool { return &v }
	cfgWith := func(shared bool) *config.City {
		return &config.City{
			Beads: config.BeadsConfig{
				Proxied:     boolp(true),
				SharedProxy: boolp(shared),
			},
			Rigs: []config.Rig{{Path: "/srv/rig-a"}},
		}
	}

	t.Run("shared_proxy off -> scope dirs only (legacy unchanged)", func(t *testing.T) {
		got := proxiedReapRoots("/srv/city", cfgWith(false))
		want := []string{
			filepath.Join("/srv/city", ".beads"),
			filepath.Join("/srv/rig-a", ".beads"),
		}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("root[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("shared_proxy on -> shared root appended", func(t *testing.T) {
		t.Setenv("BEADS_SHARED_PROXY_ROOT_PATH", "")
		t.Setenv("BEADS_SHARED_SERVER_DIR", "")
		got := proxiedReapRoots("/srv/city", cfgWith(true))
		if len(got) != 3 {
			t.Fatalf("got %v, want scope dirs + shared root", got)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("UserHomeDir: %v", err)
		}
		want := filepath.Join(home, ".beads", "shared-server", "proxy")
		if got[2] != want {
			t.Fatalf("shared root = %q, want %q", got[2], want)
		}
	})

	t.Run("shared_proxy on + BEADS_SHARED_PROXY_ROOT_PATH override", func(t *testing.T) {
		t.Setenv("BEADS_SHARED_PROXY_ROOT_PATH", "/custom/proxy-root")
		got := proxiedReapRoots("/srv/city", cfgWith(true))
		if got[len(got)-1] != "/custom/proxy-root" {
			t.Fatalf("shared root = %q, want /custom/proxy-root", got[len(got)-1])
		}
	})

	t.Run("shared_proxy on + BEADS_SHARED_SERVER_DIR override", func(t *testing.T) {
		t.Setenv("BEADS_SHARED_PROXY_ROOT_PATH", "")
		t.Setenv("BEADS_SHARED_SERVER_DIR", "/custom/shared-server")
		got := proxiedReapRoots("/srv/city", cfgWith(true))
		want := filepath.Join("/custom/shared-server", "proxy")
		if got[len(got)-1] != want {
			t.Fatalf("shared root = %q, want %q", got[len(got)-1], want)
		}
	})
}

// The shared-root child's --root IS the shared dir itself (not a path under a
// scope .beads), so the matcher's exact-match branch must catch it once
// proxiedReapRoots includes the shared root (ga-ldwvm).
func TestProxyChildPIDsFromPS_SharedRootChild(t *testing.T) {
	shared := "/home/u/.beads/shared-server/proxy"
	ps := "  97542 bd db-proxy-child --root " + shared + " --port 52447 --backend external\n"

	t.Run("matched when discovery set includes the shared root", func(t *testing.T) {
		pids := proxyChildPIDsFromPS(ps, []string{"/srv/city/.beads", shared})
		if len(pids) != 1 || pids[0] != 97542 {
			t.Fatalf("pids = %v, want [97542]", pids)
		}
	})

	t.Run("NOT matched by per-scope dirs alone (the ga-ldwvm gap)", func(t *testing.T) {
		pids := proxyChildPIDsFromPS(ps, []string{"/srv/city/.beads", "/srv/rig-a/.beads"})
		if len(pids) != 0 {
			t.Fatalf("pids = %v, want none", pids)
		}
	})
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

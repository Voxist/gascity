package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

type failingMetaProvider struct {
	runtime.Provider
	failKey string
}

func (p failingMetaProvider) SetMeta(name, key, value string) error {
	if key == p.failKey {
		return errors.New("set denied")
	}
	return p.Provider.SetMeta(name, key, value)
}

func TestRuntimeEnvWithSessionContextAlignsAgentAndBeadsActor(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		alias                string
		configuredIdentity   string
		sessionName          string
		persistedSessionName string
		origin               string
		want                 string
	}{
		{name: "canonical alias", alias: "rig/worker", sessionName: "rig--worker", persistedSessionName: "rig--worker", origin: "ephemeral", want: "rig/worker"},
		{name: "configured named identity fallback", configuredIdentity: "rig/worker", sessionName: "rig--worker", persistedSessionName: "rig--worker", origin: "ephemeral", want: "rig/worker"},
		{name: "unaliased pool-managed session claims under its bead id, not the reusable session name", sessionName: "rig--worker", persistedSessionName: "rig--worker", origin: "ephemeral", want: "session-id"},
		{name: "bead id fallback", sessionName: "s-session-id", origin: "ephemeral", want: "session-id"},
		// The origin is a table field because it SELECTS the branch: only an
		// ephemeral/pool-managed session claims under its bead ID. An unaliased
		// manual session still owns work under its persisted session_name, and
		// with every row pinned to "ephemeral" no row could say so — leaving
		// AssigneeIdentifier's session_name arm assertion-free.
		{name: "unaliased manual session keeps its persisted session name", sessionName: "rig--worker", persistedSessionName: "rig--worker", origin: "manual", want: "rig--worker"},
		{name: "aliased manual session still leads with the alias", alias: "rig/worker", sessionName: "rig--worker", persistedSessionName: "rig--worker", origin: "manual", want: "rig/worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := Info{
				ID:                      "session-id",
				SessionName:             tc.sessionName,
				SessionNameMetadata:     tc.persistedSessionName,
				Alias:                   tc.alias,
				ConfiguredNamedIdentity: tc.configuredIdentity,
				Template:                "rig/template",
				SessionOrigin:           tc.origin,
			}
			env := RuntimeEnvWithSessionContext(
				info,
				DefaultGeneration,
				DefaultContinuationEpoch,
				"instance-token",
			)

			if got := env["GC_SESSION_NAME"]; got != tc.sessionName {
				t.Fatalf("GC_SESSION_NAME = %q, want %q", got, tc.sessionName)
			}
			for _, key := range []string{"GC_AGENT", "BEADS_ACTOR"} {
				if got := env[key]; got != tc.want {
					t.Fatalf("%s = %q, want %q", key, got, tc.want)
				}
			}
			if got := AssigneeIdentifier(info); got != env["BEADS_ACTOR"] {
				t.Fatalf("AssigneeIdentifier = %q, BEADS_ACTOR = %q", got, env["BEADS_ACTOR"])
			}
		})
	}
}

func TestSyncRuntimeAliasAlignsOwnershipMetadata(t *testing.T) {
	sp := runtime.NewFake()
	assertMeta := func(sessionName, key, want string) {
		t.Helper()
		got, err := sp.GetMeta(sessionName, key)
		if err != nil {
			t.Fatalf("GetMeta(%s): %v", key, err)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	info := Info{ID: "session-id", SessionName: "rig--worker", SessionNameMetadata: "rig--worker", Alias: "rig/worker"}
	if err := SyncRuntimeAlias(sp, info); err != nil {
		t.Fatalf("SyncRuntimeAlias(set): %v", err)
	}
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "BEADS_ACTOR"} {
		assertMeta(info.SessionName, key, "rig/worker")
	}

	info.Alias = ""
	if err := SyncRuntimeAlias(sp, info); err != nil {
		t.Fatalf("SyncRuntimeAlias(clear): %v", err)
	}
	assertMeta(info.SessionName, "GC_ALIAS", "")
	for _, key := range []string{"GC_AGENT", "BEADS_ACTOR"} {
		assertMeta(info.SessionName, key, "rig--worker")
	}

	repairInfo := Info{ID: "repair-id", SessionName: "s-repair-id"}
	if err := SyncRuntimeAlias(sp, repairInfo); err != nil {
		t.Fatalf("SyncRuntimeAlias(repair): %v", err)
	}
	assertMeta(repairInfo.SessionName, "GC_ALIAS", "")
	for _, key := range []string{"GC_AGENT", "BEADS_ACTOR"} {
		assertMeta(repairInfo.SessionName, key, "repair-id")
	}
}

func TestSyncRuntimeAliasRollsBackOnPartialFailure(t *testing.T) {
	base := runtime.NewFake()
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "BEADS_ACTOR"} {
		if err := base.SetMeta("rig--worker", key, "old-alias"); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	sp := failingMetaProvider{Provider: base, failKey: "BEADS_ACTOR"}
	info := Info{ID: "session-id", SessionName: "rig--worker", SessionNameMetadata: "rig--worker", Alias: "new-alias"}

	if err := SyncRuntimeAlias(sp, info); err == nil {
		t.Fatal("SyncRuntimeAlias() error = nil, want BEADS_ACTOR failure")
	}
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "BEADS_ACTOR"} {
		got, err := base.GetMeta("rig--worker", key)
		if err != nil {
			t.Fatalf("GetMeta(%s): %v", key, err)
		}
		if got != "old-alias" {
			t.Fatalf("%s after rollback = %q, want old-alias", key, got)
		}
	}
}

// ConfigEnvExpansionIdentity hands config-env expansion the same identity
// RuntimeEnvWithSessionContext binds, so a ${GC_SESSION_NAME} or ${GC_AGENT}
// in city.toml expands to what the child session actually runs with.
func TestConfigEnvExpansionIdentityMatchesRuntimeEnv(t *testing.T) {
	for _, info := range []Info{
		{ID: "gc-1", SessionName: "worker-9", Alias: "rig/worker"},
		{ID: "gc-2", SessionName: "named-7", ConfiguredNamedIdentity: "rig/named"},
		{ID: "gc-3", SessionName: "s-gc-3"},
	} {
		got := ConfigEnvExpansionIdentity(info)
		runtimeEnv := RuntimeEnvWithSessionContext(info, 0, 0, "")
		for _, key := range []string{"GC_SESSION_NAME", "GC_AGENT"} {
			if got[key] != runtimeEnv[key] {
				t.Errorf("%+v: ConfigEnvExpansionIdentity[%s] = %q, want runtime value %q", info, key, got[key], runtimeEnv[key])
			}
		}
	}
}

// Identity not known yet (a create whose session name the bead ID will
// generate) is present and empty, so expansion never falls back to the
// controller process's own identity.
func TestConfigEnvExpansionIdentityPinsUnknownIdentityEmpty(t *testing.T) {
	got := ConfigEnvExpansionIdentity(Info{})
	for _, key := range []string{"GC_SESSION_NAME", "GC_AGENT"} {
		if v, ok := got[key]; !ok || v != "" {
			t.Errorf("ConfigEnvExpansionIdentity(Info{})[%s] = %q (present=%v), want present and empty", key, v, ok)
		}
	}
}

func TestConfigEnvExpansionIdentityForCreate(t *testing.T) {
	for _, tc := range []struct {
		alias, explicitName    string
		wantSession, wantAgent string
	}{
		{alias: "rig/worker", explicitName: "worker-7", wantSession: "worker-7", wantAgent: "rig/worker"},
		{explicitName: " worker-7 ", wantSession: "worker-7", wantAgent: "worker-7"},
		{alias: "rig/worker", wantSession: "", wantAgent: "rig/worker"},
		{},
	} {
		got := ConfigEnvExpansionIdentityForCreate(tc.alias, tc.explicitName)
		if got["GC_SESSION_NAME"] != tc.wantSession || got["GC_AGENT"] != tc.wantAgent {
			t.Errorf("ConfigEnvExpansionIdentityForCreate(%q, %q) = %v, want GC_SESSION_NAME=%q GC_AGENT=%q", tc.alias, tc.explicitName, got, tc.wantSession, tc.wantAgent)
		}
		for _, key := range []string{"GC_SESSION_NAME", "GC_AGENT"} {
			if _, ok := got[key]; !ok {
				t.Errorf("ConfigEnvExpansionIdentityForCreate(%q, %q) omits %s; want present", tc.alias, tc.explicitName, key)
			}
		}
	}
}

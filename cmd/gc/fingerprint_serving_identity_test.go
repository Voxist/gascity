package main

import (
	"encoding/json"
	"io"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// servingEnv is the [providers.tier-standard].env shape an external failover
// reconciler rewrites in place: a vendor (base URL + every model key) plus the
// credentials for it.
func servingEnv(baseURL, model, token string) map[string]string {
	return map[string]string{
		"ANTHROPIC_BASE_URL":             baseURL,
		"ANTHROPIC_MODEL":                model,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   model,
		"ANTHROPIC_DEFAULT_SONNET_MODEL": model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  model,
		"ANTHROPIC_SMALL_FAST_MODEL":     model,
		"ANTHROPIC_AUTH_TOKEN":           token,
		"ANTHROPIC_API_KEY":              token,
		"OTEL_LOGS_EXPORTER":             "otlp",
	}
}

const (
	zaiURL       = "https://api.z.ai/api/anthropic"
	zaiModel     = "glm-5.3-flash"
	anthropicURL = "https://api.anthropic.com"
	sonnetModel  = "claude-sonnet-5"
)

// resolveServingConfig resolves an agent in the city at cityPath on a
// same-named provider whose env is providerEnv (and, when upstream is non-nil,
// an [upstreams.primary] the agent selects), and returns its runtime config as
// the reconciler hashes it.
func resolveServingConfig(t *testing.T, cityPath string, providerEnv map[string]string, upstream *config.UpstreamSpec) runtime.Config {
	t.Helper()
	params := &agentBuildParams{
		cityName:  "city",
		cityPath:  cityPath,
		workspace: &config.Workspace{Provider: "tier-standard"},
		providers: map[string]config.ProviderSpec{"tier-standard": {
			Command:     "echo",
			PromptMode:  "none",
			Env:         providerEnv,
			UpstreamEnv: config.UpstreamEnvBinding{BaseURL: "ANTHROPIC_BASE_URL", AuthToken: "ANTHROPIC_AUTH_TOKEN"},
		}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{Name: "executor"}
	if upstream != nil {
		params.city = &config.City{Upstreams: map[string]config.UpstreamSpec{"primary": *upstream}}
		agent.Upstream = "primary"
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	return templateParamsToConfig(tp)
}

// TestResolvedServingIdentityDrivesLaunchFingerprint runs the production
// resolution path (resolveTemplate -> templateParamsToConfig) for the shapes a
// failover writes, and asserts which fingerprint halves move.
func TestResolvedServingIdentityDrivesLaunchFingerprint(t *testing.T) {
	type moves struct{ core, launch, provision bool }
	cases := []struct {
		name          string
		before, after func(t *testing.T, cityPath string) runtime.Config
		want          moves
	}{
		{
			name: "provider block flips vendor in place",
			before: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, servingEnv(zaiURL, zaiModel, "tok-1"), nil)
			},
			after: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, servingEnv(anthropicURL, sonnetModel, "tok-1"), nil)
			},
			want: moves{core: true, launch: true},
		},
		{
			name: "provider block changes only the model",
			before: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, servingEnv(zaiURL, zaiModel, "tok-1"), nil)
			},
			after: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, servingEnv(zaiURL, sonnetModel, "tok-1"), nil)
			},
			want: moves{core: true, launch: true},
		},
		{
			name: "provider block rotates only the credentials",
			before: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, servingEnv(zaiURL, zaiModel, "tok-1"), nil)
			},
			after: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, servingEnv(zaiURL, zaiModel, "tok-2"), nil)
			},
			want: moves{},
		},
		{
			name: "provider credential rotated behind a $VAR reference",
			before: func(t *testing.T, cityPath string) runtime.Config {
				t.Setenv("GC_TEST_SERVING_TOKEN", "tok-1")
				return resolveServingConfig(t, cityPath, servingEnv(zaiURL, zaiModel, "$GC_TEST_SERVING_TOKEN"), nil)
			},
			after: func(t *testing.T, cityPath string) runtime.Config {
				t.Setenv("GC_TEST_SERVING_TOKEN", "tok-2")
				return resolveServingConfig(t, cityPath, servingEnv(zaiURL, zaiModel, "$GC_TEST_SERVING_TOKEN"), nil)
			},
			want: moves{},
		},
		{
			name: "same-named upstream repointed in place",
			before: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, nil, &config.UpstreamSpec{BaseURL: zaiURL, AuthToken: "tok-1"})
			},
			after: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, nil, &config.UpstreamSpec{BaseURL: anthropicURL, AuthToken: "tok-1"})
			},
			want: moves{core: true, launch: true},
		},
		{
			name: "same-named upstream rotates only its token",
			before: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, nil, &config.UpstreamSpec{BaseURL: zaiURL, AuthToken: "tok-1"})
			},
			after: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, nil, &config.UpstreamSpec{BaseURL: zaiURL, AuthToken: "tok-2"})
			},
			want: moves{},
		},
		{
			name: "unrelated operator-authored env changes as before",
			before: func(t *testing.T, cityPath string) runtime.Config {
				return resolveServingConfig(t, cityPath, servingEnv(zaiURL, zaiModel, "tok-1"), nil)
			},
			after: func(t *testing.T, cityPath string) runtime.Config {
				env := servingEnv(zaiURL, zaiModel, "tok-1")
				env["OTEL_LOGS_EXPORTER"] = "none"
				return resolveServingConfig(t, cityPath, env, nil)
			},
			want: moves{core: true, launch: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeTemplateResolveCityConfig(t, cityPath, "file")
			a, b := tc.before(t, cityPath), tc.after(t, cityPath)
			got := moves{
				core:      runtime.CoreFingerprint(a) != runtime.CoreFingerprint(b),
				launch:    runtime.LaunchFingerprint(a) != runtime.LaunchFingerprint(b),
				provision: runtime.ProvisionFingerprint(a) != runtime.ProvisionFingerprint(b),
			}
			if got != tc.want {
				t.Errorf("fingerprint moves = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// servingDriftEnv registers a running "worker" whose desired config serves
// from currentEnv and returns the reconciler's current hash config plus the
// same config as it was when the session started on startedEnv.
func servingDriftEnv(t *testing.T, startedEnv, currentEnv map[string]string) (*reconcilerTestEnv, beads.Bead, runtime.Config, runtime.Config) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	tp := env.desiredState["worker"]
	tp.Env = maps.Clone(currentEnv)
	tp.OperatorEnv = maps.Clone(currentEnv)
	env.desiredState["worker"] = tp
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	current := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	started := current
	started.Env = maps.Clone(startedEnv)
	started.OperatorEnv = maps.Clone(startedEnv)
	return env, session, current, started
}

func stampStartedHashes(t *testing.T, env *reconcilerTestEnv, session *beads.Bead, started runtime.Config, version string) {
	t.Helper()
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(started))
	if err != nil {
		t.Fatal(err)
	}
	reversion := func(h string) string {
		return version + strings.TrimPrefix(h, runtime.FingerprintVersion)
	}
	env.setSessionMetadata(session, map[string]string{
		"started_config_hash":    reversion(runtime.CoreFingerprint(started)),
		"started_provision_hash": reversion(runtime.ProvisionFingerprint(started)),
		"started_launch_hash":    reversion(runtime.LaunchFingerprint(started)),
		"core_hash_breakdown":    string(breakdown),
	})
}

// The production incident: [providers.tier-standard] is rewritten from z.ai to
// Anthropic under the same name. The running session must take the config-drift
// path — as a launch-only drift, a warm-box relaunch onto the new vendor.
func TestReconcileSessionBeads_ProviderVendorFlipRelaunchesOntoNewVendor(t *testing.T) {
	env, session, current, started := servingDriftEnv(t,
		servingEnv(zaiURL, zaiModel, "tok-1"), servingEnv(anthropicURL, sonnetModel, "tok-1"))
	stampStartedHashes(t, env, &session, started, runtime.FingerprintVersion)

	env.reconcile([]beads.Bead{session})

	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Fatalf("Relaunch calls = %d, want 1 (a vendor flip is launch-only config drift); stderr=%s", got, env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "config-drift worker") {
		t.Errorf("stderr does not report config-drift for the flip:\n%s", env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "ServingIdentity") {
		t.Errorf("drift diagnostics do not name ServingIdentity:\n%s", env.stderr.String())
	}
	if strings.Contains(env.stderr.String(), "tok-1") {
		t.Errorf("drift diagnostics leak a credential:\n%s", env.stderr.String())
	}
	if rc := env.sp.LastRelaunchConfig("worker"); rc == nil {
		t.Error("no Relaunch config recorded")
	} else if got := rc.Env["ANTHROPIC_BASE_URL"]; got != anthropicURL {
		t.Errorf("relaunched ANTHROPIC_BASE_URL = %q, want %q", got, anthropicURL)
	}
	b, _ := env.store.Get(session.ID)
	if got, want := b.Metadata["started_launch_hash"], runtime.LaunchFingerprint(current); got != want {
		t.Errorf("started_launch_hash = %q, want rebaselined %q", got, want)
	}
}

// A credential rotation in the same provider block moves no fingerprint: no
// relaunch, no drain, no stop.
func TestReconcileSessionBeads_ProviderCredentialRotationLeavesSessionAlone(t *testing.T) {
	env, session, _, started := servingDriftEnv(t,
		servingEnv(zaiURL, zaiModel, "tok-1"), servingEnv(zaiURL, zaiModel, "tok-2"))
	stampStartedHashes(t, env, &session, started, runtime.FingerprintVersion)

	env.reconcile([]beads.Bead{session})

	for _, method := range []string{"Relaunch", "Stop"} {
		if got := env.sp.CountCalls(method, "worker"); got != 0 {
			t.Errorf("%s calls = %d, want 0 after a credential-only rotation; stderr=%s", method, got, env.stderr.String())
		}
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("credential rotation began a %q drain", ds.reason)
	}
	if strings.Contains(env.stderr.String(), "config-drift") {
		t.Errorf("credential rotation reported config-drift:\n%s", env.stderr.String())
	}
}

// Upgrade path: hashes stamped by the previous binary carry v6. The first tick
// on v7 rebaselines them silently — no relaunch, no drain — even when the v7
// preimage differs (serving identity now hashed, credentials dropped). This is
// what keeps the version bump from relaunching the whole fleet at deploy. It
// also means a provider flip that is still PENDING on a session at upgrade time
// is absorbed into the new baseline rather than acted on.
func TestReconcileSessionBeads_PreviousVersionHashesRebaselineWithoutRelaunch(t *testing.T) {
	env, session, current, started := servingDriftEnv(t,
		servingEnv(zaiURL, zaiModel, "tok-1"), servingEnv(zaiURL, zaiModel, "tok-1"))
	stampStartedHashes(t, env, &session, started, "v6")

	env.reconcile([]beads.Bead{session})

	for _, method := range []string{"Relaunch", "Stop"} {
		if got := env.sp.CountCalls(method, "worker"); got != 0 {
			t.Errorf("%s calls = %d, want 0 on a version rebaseline; stderr=%s", method, got, env.stderr.String())
		}
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("version rebaseline began a %q drain", ds.reason)
	}
	if !strings.Contains(env.stderr.String(), "rebaselined legacy hash for worker") {
		t.Errorf("stderr does not report the rebaseline:\n%s", env.stderr.String())
	}
	b, _ := env.store.Get(session.ID)
	for key, want := range map[string]string{
		"started_config_hash":    runtime.CoreFingerprint(current),
		"started_provision_hash": runtime.ProvisionFingerprint(current),
		"started_launch_hash":    runtime.LaunchFingerprint(current),
	} {
		if got := b.Metadata[key]; got != want {
			t.Errorf("%s = %q, want rebaselined %q", key, got, want)
		}
	}
}

package main

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// fakeRotateTmux is a test spy satisfying RotateTmux.
type fakeRotateTmux struct {
	// sessions maps session name → env vars
	sessions map[string]map[string]string
	// globalEnv holds tmux global env vars
	globalEnv map[string]string
	// callsSet records (key, value) calls to SetGlobalEnvironment
	callsSet []string
	// errListSessions, if non-nil, is returned by ListSessions.
	errListSessions error
}

func newFakeRotateTmux(sessions map[string]map[string]string) *fakeRotateTmux {
	return &fakeRotateTmux{
		sessions:  sessions,
		globalEnv: make(map[string]string),
	}
}

func (f *fakeRotateTmux) SetGlobalEnvironment(key, value string) error {
	f.globalEnv[key] = value
	f.callsSet = append(f.callsSet, key)
	return nil
}

func (f *fakeRotateTmux) ListSessions() ([]string, error) {
	if f.errListSessions != nil {
		return nil, f.errListSessions
	}
	names := make([]string, 0, len(f.sessions))
	for name := range f.sessions {
		names = append(names, name)
	}
	return names, nil
}

func (f *fakeRotateTmux) GetEnvironment(session, key string) (string, error) {
	env, ok := f.sessions[session]
	if !ok {
		return "", nil
	}
	return env[key], nil
}

func (f *fakeRotateTmux) SetEnvironment(session, key, value string) error {
	if f.sessions[session] == nil {
		f.sessions[session] = make(map[string]string)
	}
	f.sessions[session][key] = value
	return nil
}

func TestRotateProviderKey(t *testing.T) {
	spec := ProviderEnv{Env: map[string]string{
		"ANTHROPIC_API_KEY":    "${ANTHROPIC_AUTH_TOKEN_ZAI}",
		"ANTHROPIC_AUTH_TOKEN": "${ANTHROPIC_AUTH_TOKEN_ZAI}",
	}}

	t.Run("rotates zai sessions only", func(t *testing.T) {
		fake := newFakeRotateTmux(map[string]map[string]string{
			"session-zai":    {"GC_PROVIDER": "zai"},
			"session-claude": {"GC_PROVIDER": "claude"},
		})

		result, err := rotateProviderKey(context.Background(), "zai", "sk-ant-new", fake, spec, false)
		if err != nil {
			t.Fatalf("rotateProviderKey: %v", err)
		}

		// Global source var updated.
		if fake.globalEnv["ANTHROPIC_AUTH_TOKEN_ZAI"] != "sk-ant-new" {
			t.Errorf("global ANTHROPIC_AUTH_TOKEN_ZAI = %q; want %q", fake.globalEnv["ANTHROPIC_AUTH_TOKEN_ZAI"], "sk-ant-new")
		}

		// zai session has expanded vars updated.
		if fake.sessions["session-zai"]["ANTHROPIC_API_KEY"] != "sk-ant-new" {
			t.Errorf("session-zai ANTHROPIC_API_KEY = %q; want %q", fake.sessions["session-zai"]["ANTHROPIC_API_KEY"], "sk-ant-new")
		}
		if fake.sessions["session-zai"]["ANTHROPIC_AUTH_TOKEN"] != "sk-ant-new" {
			t.Errorf("session-zai ANTHROPIC_AUTH_TOKEN = %q; want %q", fake.sessions["session-zai"]["ANTHROPIC_AUTH_TOKEN"], "sk-ant-new")
		}

		// claude session untouched (only GC_PROVIDER key exists).
		if got := fake.sessions["session-claude"]["ANTHROPIC_API_KEY"]; got != "" {
			t.Errorf("session-claude ANTHROPIC_API_KEY = %q; want untouched (empty)", got)
		}

		if !slices.Contains(result.GlobalVarsUpdated, "ANTHROPIC_AUTH_TOKEN_ZAI") {
			t.Errorf("GlobalVarsUpdated = %v; want to contain ANTHROPIC_AUTH_TOKEN_ZAI", result.GlobalVarsUpdated)
		}
		if !slices.Contains(result.SessionsUpdated, "session-zai") {
			t.Errorf("SessionsUpdated = %v; want to contain session-zai", result.SessionsUpdated)
		}
		if slices.Contains(result.SessionsUpdated, "session-claude") {
			t.Errorf("SessionsUpdated = %v; should not contain session-claude", result.SessionsUpdated)
		}
	})

	t.Run("dry-run skips writes", func(t *testing.T) {
		fake := newFakeRotateTmux(map[string]map[string]string{
			"session-zai": {"GC_PROVIDER": "zai"},
		})

		result, err := rotateProviderKey(context.Background(), "zai", "sk-ant-new", fake, spec, true)
		if err != nil {
			t.Fatalf("rotateProviderKey dry-run: %v", err)
		}

		// No actual writes.
		if len(fake.callsSet) != 0 {
			t.Errorf("dry-run: SetGlobalEnvironment called %d times; want 0", len(fake.callsSet))
		}
		if fake.sessions["session-zai"]["ANTHROPIC_API_KEY"] != "" {
			t.Errorf("dry-run: session env modified; want untouched")
		}

		// But result still reports what would change.
		if !slices.Contains(result.GlobalVarsUpdated, "ANTHROPIC_AUTH_TOKEN_ZAI") {
			t.Errorf("dry-run GlobalVarsUpdated = %v; want ANTHROPIC_AUTH_TOKEN_ZAI", result.GlobalVarsUpdated)
		}
		if !slices.Contains(result.SessionsUpdated, "session-zai") {
			t.Errorf("dry-run SessionsUpdated = %v; want session-zai", result.SessionsUpdated)
		}
	})
}

// TestRotateProviderKeyMixedStaticRef guards against corrupting static-literal
// keys in spec.Env. When a provider spec has both a ${VAR}-ref key and a
// static-literal key (e.g. ANTHROPIC_BASE_URL=https://...), only the ref-
// bearing key should be updated in the session — the static key must be left
// unchanged.
func TestRotateProviderKeyMixedStaticRef(t *testing.T) {
	spec := ProviderEnv{Env: map[string]string{
		"ANTHROPIC_API_KEY":  "${ANTHROPIC_AUTH_TOKEN_ZAI}", // ref-bearing
		"ANTHROPIC_BASE_URL": "https://api.example.com",     // static literal
	}}

	fake := newFakeRotateTmux(map[string]map[string]string{
		"session-zai": {
			"GC_PROVIDER":        "zai",
			"ANTHROPIC_BASE_URL": "https://api.example.com",
		},
	})

	_, err := rotateProviderKey(context.Background(), "zai", "sk-ant-new", fake, spec, false)
	if err != nil {
		t.Fatalf("rotateProviderKey: %v", err)
	}

	// Ref-bearing key must be updated.
	if fake.sessions["session-zai"]["ANTHROPIC_API_KEY"] != "sk-ant-new" {
		t.Errorf("ANTHROPIC_API_KEY = %q; want %q", fake.sessions["session-zai"]["ANTHROPIC_API_KEY"], "sk-ant-new")
	}
	// Static-literal key must NOT be overwritten with newKey.
	if got := fake.sessions["session-zai"]["ANTHROPIC_BASE_URL"]; got != "https://api.example.com" {
		t.Errorf("ANTHROPIC_BASE_URL = %q; want unchanged %q", got, "https://api.example.com")
	}
}

// TestRotateProviderKeyListSessionsError verifies that a ListSessions failure
// is returned to the caller rather than swallowed. If the global env was already
// written and ListSessions fails, the operator must see the error so they can
// retry rather than believe rotation succeeded with zero sessions updated.
func TestRotateProviderKeyListSessionsError(t *testing.T) {
	spec := ProviderEnv{Env: map[string]string{
		"ANTHROPIC_API_KEY": "${ANTHROPIC_AUTH_TOKEN_ZAI}",
	}}

	sentinelErr := errors.New("tmux: server not running")
	fake := newFakeRotateTmux(nil)
	fake.errListSessions = sentinelErr

	_, err := rotateProviderKey(context.Background(), "zai", "sk-ant-new", fake, spec, false)
	if err == nil {
		t.Fatal("expected error from ListSessions, got nil")
	}
	if !errors.Is(err, sentinelErr) {
		t.Errorf("err = %v; want to wrap %v", err, sentinelErr)
	}
}

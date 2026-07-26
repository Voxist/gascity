package config

import (
	"reflect"
	"sort"
	"testing"
)

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestNativeStoreCanaryScopeSet(t *testing.T) {
	tests := []struct {
		name     string
		scopes   []string
		envValue string
		want     []string
	}{
		{
			name: "empty config and empty env is OFF",
			want: []string{},
		},
		{
			name:   "config scopes only",
			scopes: []string{"vr", "fe"},
			want:   []string{"fe", "vr"},
		},
		{
			name:     "env scopes only",
			envValue: "vr,hq",
			want:     []string{"hq", "vr"},
		},
		{
			name:     "config and env union and de-dupe",
			scopes:   []string{"vr"},
			envValue: "vr,hq",
			want:     []string{"hq", "vr"},
		},
		{
			name:     "blank and whitespace names dropped",
			scopes:   []string{"  ", "vr"},
			envValue: " , hq , ",
			want:     []string{"hq", "vr"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := BeadsConfig{NativeStoreCanaryScopes: tc.scopes}
			got := sortedKeys(b.NativeStoreCanaryScopeSet(tc.envValue))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NativeStoreCanaryScopeSet(%q) = %v, want %v", tc.envValue, got, tc.want)
			}
		})
	}
}

func TestNativeStoreCanaryEnabledForScope(t *testing.T) {
	tests := []struct {
		name     string
		scopes   []string
		envValue string
		scope    string
		want     bool
	}{
		{
			name:  "default off when unset",
			scope: "vr",
			want:  false,
		},
		{
			name:   "enabled via config",
			scopes: []string{"vr"},
			scope:  "vr",
			want:   true,
		},
		{
			name:     "enabled via env override",
			envValue: "vr",
			scope:    "vr",
			want:     true,
		},
		{
			name:   "other scopes stay off (additive)",
			scopes: []string{"vr"},
			scope:  "hq",
			want:   false,
		},
		{
			name:   "scope name is trimmed before compare",
			scopes: []string{"vr"},
			scope:  "  vr  ",
			want:   true,
		},
		{
			name:  "blank scope is never enabled",
			scope: "   ",
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := BeadsConfig{NativeStoreCanaryScopes: tc.scopes}
			if got := b.NativeStoreCanaryEnabledForScope(tc.scope, tc.envValue); got != tc.want {
				t.Errorf("NativeStoreCanaryEnabledForScope(%q, %q) = %v, want %v", tc.scope, tc.envValue, got, tc.want)
			}
		})
	}
}

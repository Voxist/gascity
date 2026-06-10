package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestApplyNativeStoreCanaryEnv(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		scope     string
		enabled   bool
		wantMode  string // expected BEADS_DOLT_SERVER_MODE, "" means absent
		wantErr   bool
		errSubstr string
	}{
		{
			name:     "disabled is a no-op leaving env unchanged",
			env:      map[string]string{"BEADS_DOLT_SERVER_HOST": "127.0.0.1", "BEADS_DOLT_SERVER_PORT": "3306"},
			scope:    "vr",
			enabled:  false,
			wantMode: "",
		},
		{
			name:     "enabled with resolved endpoint projects server mode",
			env:      map[string]string{"BEADS_DOLT_SERVER_HOST": "127.0.0.1", "BEADS_DOLT_SERVER_PORT": "3306"},
			scope:    "vr",
			enabled:  true,
			wantMode: nativeStoreCanaryServerModeValue,
		},
		{
			name:      "enabled with missing host errors loudly",
			env:       map[string]string{"BEADS_DOLT_SERVER_PORT": "3306"},
			scope:     "vr",
			enabled:   true,
			wantErr:   true,
			errSubstr: "unresolvable",
		},
		{
			name:      "enabled with blank port errors loudly",
			env:       map[string]string{"BEADS_DOLT_SERVER_HOST": "127.0.0.1", "BEADS_DOLT_SERVER_PORT": "  "},
			scope:     "vr",
			enabled:   true,
			wantErr:   true,
			errSubstr: "unresolvable",
		},
		{
			name:      "enabled with nil env errors",
			env:       nil,
			scope:     "vr",
			enabled:   true,
			wantErr:   true,
			errSubstr: "env map is nil",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var before map[string]string
			if tc.env != nil {
				before = make(map[string]string, len(tc.env))
				for k, v := range tc.env {
					before[k] = v
				}
			}
			err := applyNativeStoreCanaryEnv(tc.env, tc.scope, tc.enabled)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("applyNativeStoreCanaryEnv() error = nil, want error")
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
				if !strings.Contains(err.Error(), tc.scope) {
					t.Errorf("error %q should mention scope %q", err.Error(), tc.scope)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyNativeStoreCanaryEnv() unexpected error: %v", err)
			}
			if !tc.enabled {
				if !reflect.DeepEqual(tc.env, before) {
					t.Errorf("disabled lever mutated env: got %v, want %v", tc.env, before)
				}
				return
			}
			if got := tc.env["BEADS_DOLT_SERVER_MODE"]; got != tc.wantMode {
				t.Errorf("BEADS_DOLT_SERVER_MODE = %q, want %q", got, tc.wantMode)
			}
		})
	}
}

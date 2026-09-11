package main

import (
	"strings"
	"testing"
)

func TestConventionalMetadataValueRefusal(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    bool
		wantMsg string // substring of the refusal; empty when want is false
	}{
		{
			name:    "empty route value is refused",
			args:    []string{"update", "bd-1", "--set-metadata", "gc.routed_to="},
			want:    true,
			wantMsg: "unset-metadata gc.routed_to",
		},
		{
			name:    "awaiting_human identity value is refused",
			args:    []string{"update", "bd-1", "--set-metadata", "gc.awaiting_human=karel@voxist.com"},
			want:    true,
			wantMsg: "gc.human_owner",
		},
		{
			name: "two-token and inline forms are both scanned",
			args: []string{"--actor", "bot", "update", "bd-1", "--set-metadata=gc.awaiting_human=1", "--set-metadata", "gc.routed_to="},
			want: true,
		},
		{
			name: "legal values pass",
			args: []string{"update", "bd-1", "--set-metadata", "gc.routed_to=voxist.platform-architect", "--set-metadata", "gc.awaiting_human=true"},
			want: false,
		},
		{
			name: "unconventioned keys stay open-world",
			args: []string{"update", "bd-1", "--set-metadata", "gc.human_ask="},
			want: false,
		},
		{
			name: "other verbs are untouched",
			args: []string{"create", "a bead", "--set-metadata", "gc.routed_to="},
			want: false,
		},
		{
			name: "valueless flag is skipped, not judged",
			args: []string{"update", "bd-1", "--set-metadata"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, got := conventionalMetadataValueRefusal(tt.args)
			if got != tt.want {
				t.Fatalf("conventionalMetadataValueRefusal(%q) reported %v, want %v (msg: %q)", tt.args, got, tt.want, msg)
			}
			if tt.want && !strings.Contains(msg, tt.wantMsg) {
				t.Fatalf("refusal = %q, want substring %q", msg, tt.wantMsg)
			}
		})
	}
}

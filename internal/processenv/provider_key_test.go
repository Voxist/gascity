package processenv_test

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/processenv"
)

func TestProviderSourceVars(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{
			name: "single ref",
			env:  map[string]string{"ANTHROPIC_API_KEY": "${ANTHROPIC_AUTH_TOKEN_ZAI}", "X": "literal"},
			want: []string{"ANTHROPIC_AUTH_TOKEN_ZAI"},
		},
		{
			name: "empty map",
			env:  map[string]string{},
			want: []string{},
		},
		{
			name: "nil map",
			env:  nil,
			want: []string{},
		},
		{
			name: "no refs",
			env:  map[string]string{"ANTHROPIC_API_KEY": "sk-ant-literal"},
			want: []string{},
		},
		{
			name: "duplicate ref deduplicated",
			env: map[string]string{
				"ANTHROPIC_API_KEY":    "${ANTHROPIC_AUTH_TOKEN_ZAI}",
				"ANTHROPIC_AUTH_TOKEN": "${ANTHROPIC_AUTH_TOKEN_ZAI}",
			},
			want: []string{"ANTHROPIC_AUTH_TOKEN_ZAI"},
		},
		{
			name: "multiple distinct refs",
			env: map[string]string{
				"KEY_A": "${VAR_ONE}",
				"KEY_B": "${VAR_TWO}",
			},
			want: []string{"VAR_ONE", "VAR_TWO"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := processenv.ProviderSourceVars(tt.env)
			slices.Sort(got)
			slices.Sort(tt.want)
			if len(got) != len(tt.want) {
				t.Fatalf("ProviderSourceVars(%v) = %v; want %v", tt.env, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("ProviderSourceVars(%v)[%d] = %q; want %q", tt.env, i, got[i], tt.want[i])
				}
			}
		})
	}
}

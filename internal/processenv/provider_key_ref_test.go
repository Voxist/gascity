package processenv_test

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/processenv"
)

// TestSoleEnvRef pins the grammar SoleEnvRef must share with session start.
//
// Session start expands config-authored env values with
// [processenv.ExpandSessionEnvValue], which is os.Expand — so it honors BOTH
// ${VAR} and bare $VAR, and it substitutes a reference in place, keeping any
// literal text around it. Any resolver that decides which variable backs a
// provider's credential has to read the value the same way, or it silently
// disagrees with the process it is describing.
func TestSoleEnvRef(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{name: "braced ref", value: "${ACME_KEY}", want: "ACME_KEY", ok: true},
		{name: "bare ref", value: "$ACME_KEY", want: "ACME_KEY", ok: true},
		{name: "static literal", value: "sk-ant-literal", ok: false},
		{name: "empty", value: "", ok: false},

		// A reference wrapped in literal text is NOT a sole reference. The
		// variable behind it holds "sk-...", not "Bearer sk-...", so writing
		// the credential to it would drop the literal — and writing the whole
		// value to it would corrupt the header on every other consumer.
		{name: "ref with literal prefix", value: "Bearer ${ACME_KEY}", ok: false},
		{name: "ref with literal suffix", value: "${ACME_KEY}-suffix", ok: false},
		{name: "two refs", value: "${ACME_ID}:${ACME_KEY}", ok: false},
		{name: "same ref twice", value: "${ACME_KEY}${ACME_KEY}", ok: false},

		// $$ parses as a reference named "$" under os.Expand. It is not a
		// legal environment variable name, so it is not a source var.
		{name: "dollar dollar", value: "$$", ok: false},
		{name: "digit-leading name", value: "${9BAD}", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := processenv.SoleEnvRef(tt.value)
			if ok != tt.ok {
				t.Fatalf("SoleEnvRef(%q) ok = %v; want %v (name %q)", tt.value, ok, tt.ok, got)
			}
			if got != tt.want {
				t.Errorf("SoleEnvRef(%q) = %q; want %q", tt.value, got, tt.want)
			}
		})
	}
}

// TestSoleEnvRefAgreesWithSessionExpansion is the anti-vacuity guard for the
// case above: for every value SoleEnvRef claims is a sole reference to VAR,
// setting VAR must make session-start expansion produce exactly that value —
// and for every value it rejects, it must be because expansion would NOT.
// This is the property that matters, checked against the real expander rather
// than against a second copy of the same regex.
func TestSoleEnvRefAgreesWithSessionExpansion(t *testing.T) {
	const secret = "sk-ant-rotated"
	for _, value := range []string{
		"${ACME_KEY}",
		"$ACME_KEY",
		"Bearer ${ACME_KEY}",
		"${ACME_KEY}-suffix",
		"sk-ant-literal",
		"",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ACME_KEY", secret)
			name, ok := processenv.SoleEnvRef(value)
			expanded := processenv.ExpandSessionEnvValue(value)
			if !ok {
				if expanded == secret {
					t.Fatalf("SoleEnvRef(%q) rejected the value, but session expansion yields exactly the secret — a rotation would have been possible and was refused", value)
				}
				return
			}
			if name != "ACME_KEY" {
				t.Fatalf("SoleEnvRef(%q) = %q; want ACME_KEY", value, name)
			}
			if expanded != secret {
				t.Fatalf("SoleEnvRef(%q) claims the value is exactly $%s, but session expansion yields %q, not %q — writing the credential to %s would corrupt this entry",
					value, name, expanded, secret, name)
			}
			if os.Getenv(name) != secret {
				t.Fatalf("test setup: %s = %q", name, os.Getenv(name))
			}
		})
	}
}

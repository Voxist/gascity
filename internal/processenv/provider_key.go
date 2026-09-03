package processenv

import (
	"os"
	"regexp"
)

// envNameRe matches a legal environment variable name. os.Expand's grammar is
// looser than this — "$$" parses as a reference named "$" — so a name it
// yields still has to be checked before it can be treated as a variable.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvName reports whether key is a legal environment variable name.
func ValidEnvName(key string) bool { return envNameRe.MatchString(key) }

// soleEnvRefMarker stands in for a reference while probing a value's shape. It
// is a byte sequence no environment value can contain, so a rendered result
// equal to it proves the value was exactly one reference and nothing else.
const soleEnvRefMarker = "\x00gc-env-ref\x00"

// SoleEnvRef reports the single environment variable that value interpolates,
// when value consists of exactly that reference and nothing else.
//
// It reads value with os.Expand — the same grammar [ExpandSessionEnvValue]
// uses at session start — so it honors both ${VAR} and bare $VAR. A resolver
// that used a narrower grammar would silently disagree with the process it
// describes: a provider declaring ANTHROPIC_API_KEY = "$ACME_KEY" would look
// like it referenced nothing at all.
//
// ok is false for a static literal, for a value referencing more than one
// variable, and for a reference surrounded by literal text such as
// "Bearer ${ACME_KEY}". Those are not failures to parse — they are values
// whose backing variable does not hold the whole value, so a caller that
// wrote a credential to the named variable would either drop the literal text
// or corrupt every other consumer of that variable. Refusing is the only
// answer that cannot silently damage the config.
func SoleEnvRef(value string) (name string, ok bool) {
	var names []string
	rendered := os.Expand(value, func(key string) string {
		names = append(names, key)
		return soleEnvRefMarker
	})
	if len(names) != 1 || rendered != soleEnvRefMarker || !envNameRe.MatchString(names[0]) {
		return "", false
	}
	return names[0], true
}

// HasEnvRef reports whether value interpolates any environment variable, under
// the same os.Expand grammar [SoleEnvRef] and [ExpandSessionEnvValue] use.
//
// It exists so a caller can tell "this value is a literal" apart from "this
// value references something, but not solely" — two situations that need
// different, actionable answers rather than one vague refusal.
func HasEnvRef(value string) bool {
	referenced := false
	os.Expand(value, func(string) string {
		referenced = true
		return ""
	})
	return referenced
}

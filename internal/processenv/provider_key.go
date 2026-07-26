package processenv

import "regexp"

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ProviderSourceVars extracts the distinct environment variable names referenced
// via ${VAR} interpolation in the values of a ProviderSpec.Env map.
// These are the "source vars" that must be updated in the tmux global env
// when a provider key is rotated.
func ProviderSourceVars(env map[string]string) []string {
	seen := make(map[string]bool)
	for _, v := range env {
		for _, m := range envRefRe.FindAllStringSubmatch(v, -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

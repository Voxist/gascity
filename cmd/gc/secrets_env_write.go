package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/processenv"
)

// upsertSecretsEnvFile sets each assignment in the dotenv file at path,
// preserving every other line — comments, blank lines, unrelated credentials
// and their order.
//
// The file is the machine-local credential source the supervisor merges into
// its service environment, so three properties matter more than brevity:
//
//   - It is operator-maintained. Comments and unrelated entries survive.
//   - A key ends up assigned exactly once. [processenv.ParseEnvFile] takes the
//     LAST assignment, so appending would leave a stale earlier line that still
//     reads as a credential to anyone opening the file.
//   - The write is atomic and private: a temp file in the same directory,
//     chmod 0600, renamed into place. A failed write leaves the supervisor's
//     current source intact rather than truncated.
//
// A value that cannot round-trip through the dotenv grammar is refused before
// anything is written. Errors never quote the value.
func upsertSecretsEnvFile(path string, assignments map[string]string) error {
	keys := make([]string, 0, len(assignments))
	for k := range assignments {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if !processenv.ValidEnvName(k) {
			return fmt.Errorf("%q is not a valid environment variable name", k)
		}
		if strings.ContainsAny(assignments[k], "\n\r") {
			return fmt.Errorf("the value for %s contains a newline, which the dotenv format cannot represent", k)
		}
	}

	var existing string
	if data, err := os.ReadFile(path); err == nil {
		existing = string(data)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	rendered := renderSecretsEnvFile(existing, keys, assignments)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	return writePrivateFileAtomic(path, []byte(rendered))
}

// renderSecretsEnvFile rewrites content so each key in keys is assigned
// exactly once, in place of its first existing assignment, with any later
// duplicate assignment of the same key removed. Keys not already present are
// appended.
func renderSecretsEnvFile(content string, keys []string, assignments map[string]string) string {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}

	var out []string
	written := make(map[string]bool, len(keys))
	trailingNewline := content == "" || strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if content == "" {
		lines = nil
	}

	for _, line := range lines {
		key, isAssignment := dotenvAssignmentKey(line)
		if !isAssignment || !want[key] {
			out = append(out, line)
			continue
		}
		if written[key] {
			// A later duplicate of a key we are setting. Drop it: keeping it
			// would shadow the line we just wrote.
			continue
		}
		out = append(out, dotenvAssignmentLine(key, assignments[key]))
		written[key] = true
	}

	for _, k := range keys {
		if !written[k] {
			out = append(out, dotenvAssignmentLine(k, assignments[k]))
		}
	}

	rendered := strings.Join(out, "\n")
	if rendered != "" && trailingNewline {
		rendered += "\n"
	}
	return rendered
}

// dotenvAssignmentKey returns the key a dotenv line assigns, mirroring
// [processenv.ParseEnvFile]'s reading of the same line. ok is false for blank
// lines, comments, and anything without a key.
func dotenvAssignmentKey(line string) (key string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	trimmed = strings.TrimPrefix(trimmed, "export ")
	k, _, found := strings.Cut(trimmed, "=")
	if !found {
		return "", false
	}
	k = strings.TrimSpace(k)
	if k == "" {
		return "", false
	}
	return k, true
}

// dotenvAssignmentLine renders one assignment that [processenv.ParseEnvFile]
// reads back as exactly value.
//
// That parser trims whitespace around the value and then strips ONE layer of
// matching surrounding quotes, so a value is quoted here whenever leaving it
// bare would lose bytes: when it has leading or trailing whitespace, or when it
// already looks quoted and would be unwrapped. Single quotes are preferred and
// double quotes are used for a value containing a single quote, because the
// parser strips whichever pair surrounds the value without interpreting escapes.
// A value containing both quote characters needs no wrapper unless whitespace
// forces one, and in that case no wrapper can survive — which the caller has
// already excluded by checking the round-trip is representable.
func dotenvAssignmentLine(key, value string) string {
	return key + "=" + dotenvQuote(value)
}

// dotenvQuote renders value so ParseEnvFile reads back exactly value.
func dotenvQuote(value string) string {
	needsWrap := value != strings.TrimSpace(value) || looksQuoted(value)
	if !needsWrap {
		return value
	}
	if !strings.Contains(value, "'") {
		return "'" + value + "'"
	}
	if !strings.Contains(value, `"`) {
		return `"` + value + `"`
	}
	return value
}

// looksQuoted reports whether ParseEnvFile would strip a quote pair from value,
// which would corrupt a credential that legitimately begins and ends with one.
func looksQuoted(value string) bool {
	if len(value) < 2 {
		return false
	}
	first, last := value[0], value[len(value)-1]
	return (first == '"' || first == '\'') && last == first
}

// writePrivateFileAtomic writes data to path through a 0600 temp file in the
// same directory, renamed into place, so a reader never observes a partial
// file and a failure leaves the previous content untouched.
func writePrivateFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) //nolint:errcheck // best-effort cleanup; a successful rename makes this a no-op

	if err := f.Chmod(0o600); err != nil {
		f.Close() //nolint:errcheck // already failing
		return fmt.Errorf("securing temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close() //nolint:errcheck // already failing
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming into %s: %w", path, err)
	}
	return nil
}

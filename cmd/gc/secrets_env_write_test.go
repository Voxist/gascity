package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/processenv"
)

// readBack parses the written file the way the supervisor does, so every
// assertion is about the value the supervisor would actually load rather than
// about the bytes we happened to write.
func readBack(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	entries, err := processenv.ParseEnvFile(string(data))
	if err != nil {
		t.Fatalf("the file we wrote does not parse as dotenv: %v\n%s", err, data)
	}
	return entries
}

func TestUpsertSecretsEnvFilePreservesEverythingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	original := "# machine-local provider credentials\n" +
		"ANTHROPIC_API_KEY=sk-ant-old\n" +
		"\n" +
		"export OPENAI_API_KEY='sk-openai-keepme'\n" +
		"# trailing comment\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := upsertSecretsEnvFile(path, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-new"}); err != nil {
		t.Fatalf("upsertSecretsEnvFile: %v", err)
	}

	got := readBack(t, path)
	if got["ANTHROPIC_API_KEY"] != "sk-ant-new" {
		t.Errorf("ANTHROPIC_API_KEY = %q; want sk-ant-new", got["ANTHROPIC_API_KEY"])
	}
	if got["OPENAI_API_KEY"] != "sk-openai-keepme" {
		t.Errorf("OPENAI_API_KEY = %q; want the untouched value — an unrelated credential must survive", got["OPENAI_API_KEY"])
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, comment := range []string{"# machine-local provider credentials", "# trailing comment"} {
		if !strings.Contains(string(raw), comment) {
			t.Errorf("comment %q was dropped; the file is operator-maintained and must survive an edit\n%s", comment, raw)
		}
	}
	if strings.Contains(string(raw), "sk-ant-old") {
		t.Errorf("the old credential is still present in the file\n%s", raw)
	}
}

func TestUpsertSecretsEnvFileCreatesPrivateFileAndDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gc-home")
	path := filepath.Join(dir, "secrets.env")

	if err := upsertSecretsEnvFile(path, map[string]string{"ACME_KEY": "sk-acme"}); err != nil {
		t.Fatalf("upsertSecretsEnvFile: %v", err)
	}

	if got := readBack(t, path)["ACME_KEY"]; got != "sk-acme" {
		t.Errorf("ACME_KEY = %q; want sk-acme", got)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %04o; want 0600 — this file holds credentials", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %04o; want 0700", perm)
	}
}

// TestUpsertSecretsEnvFileCollapsesDuplicateKeys guards a hazard of the
// obvious hand-rolled alternative (>> append): ParseEnvFile takes the LAST
// assignment, so an appended key silently shadows the earlier one and the file
// accumulates stale credentials that still look authoritative.
func TestUpsertSecretsEnvFileCollapsesDuplicateKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(path, []byte("ACME_KEY=first\nOTHER=keep\nACME_KEY=second\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := upsertSecretsEnvFile(path, map[string]string{"ACME_KEY": "sk-final"}); err != nil {
		t.Fatalf("upsertSecretsEnvFile: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "ACME_KEY="); n != 1 {
		t.Errorf("ACME_KEY appears %d times; want exactly 1 — a stale duplicate still reads as a credential\n%s", n, raw)
	}
	got := readBack(t, path)
	if got["ACME_KEY"] != "sk-final" {
		t.Errorf("ACME_KEY = %q; want sk-final", got["ACME_KEY"])
	}
	if got["OTHER"] != "keep" {
		t.Errorf("OTHER = %q; want keep", got["OTHER"])
	}
}

// TestUpsertSecretsEnvFileRoundTripsAwkwardValues is the anti-vacuity guard on
// the writer: whatever quoting it chooses, ParseEnvFile must read back exactly
// the credential we asked it to store. A credential that round-trips wrong is
// an authentication failure the operator cannot see in the file.
func TestUpsertSecretsEnvFileRoundTripsAwkwardValues(t *testing.T) {
	values := map[string]string{
		"plain":          "sk-ant-abc123",
		"with_equals":    "sk=ant=abc",
		"with_hash":      "sk-ant#notacomment",
		"with_spaces":    "sk ant abc",
		"lead_trail_ws":  "  sk-ant-padded  ",
		"with_squote":    "sk-ant-o'brien",
		"with_dquote":    `sk-ant-"quoted"`,
		"dollar_literal": "sk-ant-$NOT_A_REF",
		"looks_quoted":   `"sk-ant-wrapped"`,
	}
	for name, want := range values {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secrets.env")
			key := "ACME_KEY"
			if err := upsertSecretsEnvFile(path, map[string]string{key: want}); err != nil {
				t.Fatalf("upsertSecretsEnvFile(%q): %v", want, err)
			}
			if got := readBack(t, path)[key]; got != want {
				t.Errorf("round-trip of %q gave %q", want, got)
			}
		})
	}
}

// TestUpsertSecretsEnvFileRefusesUnrepresentableValue: dotenv is line-based, so
// a newline in a credential cannot round-trip. Refuse loudly rather than write
// a file that parses into a truncated key.
func TestUpsertSecretsEnvFileRefusesUnrepresentableValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	err := upsertSecretsEnvFile(path, map[string]string{"ACME_KEY": "sk-ant\nINJECTED=evil"})
	if err == nil {
		t.Fatal("a newline-bearing value was accepted; it cannot round-trip and would inject a second assignment")
	}
	if strings.Contains(err.Error(), "sk-ant") {
		t.Errorf("the error echoes the credential: %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("a file was created despite the refusal")
	}
}

// TestUpsertSecretsEnvFileLeavesOriginalOnFailure pins the atomic contract: a
// refused write must not have truncated the file the supervisor still reads.
func TestUpsertSecretsEnvFileLeavesOriginalOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	original := "ACME_KEY=sk-ant-still-good\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := upsertSecretsEnvFile(path, map[string]string{"ACME_KEY": "bad\nvalue"}); err == nil {
		t.Fatal("expected refusal")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != original {
		t.Errorf("file changed on a refused write:\ngot  %q\nwant %q", raw, original)
	}
}

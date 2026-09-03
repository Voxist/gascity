package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/processenv"
)

// zaiCityTOML is the shape our own city runs and the shape the withdrawn
// rotate-key command corrupted: a provider whose base URL AND credential are
// both env refs, so a resolver that does not distinguish them overwrites the
// endpoint with the API key.
const zaiCityTOML = `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.zai]
base = "builtin:claude"
env = {ANTHROPIC_BASE_URL = "${ZAI_BASE_URL}", ANTHROPIC_AUTH_TOKEN = "${ANTHROPIC_AUTH_TOKEN_ZAI}", ANTHROPIC_API_KEY = "${ANTHROPIC_AUTH_TOKEN_ZAI}"}
`

// newCredentialsTestEnv points GC_HOME at a temp dir and clears the variables
// the command inspects, so an operator's real shell cannot change the result.
func newCredentialsTestEnv(t *testing.T) (cityPath, secretsPath string) {
	t.Helper()
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("ANTHROPIC_AUTH_TOKEN_ZAI", "")
	t.Setenv("ZAI_BASE_URL", "")
	return writeProviderTestCity(t, zaiCityTOML), filepath.Join(gcHome, "secrets.env")
}

// TestProviderCredentialsReportsCredentialSourceOnly is the CLI-level guard on
// the defect that made rotate-key unsafe. It goes through run(), so it covers
// the path the operator actually takes.
func TestProviderCredentialsReportsCredentialSourceOnly(t *testing.T) {
	cityPath, _ := newCredentialsTestEnv(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "zai"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()

	if !strings.Contains(out, "ANTHROPIC_AUTH_TOKEN_ZAI") {
		t.Errorf("output does not name the credential's source variable:\n%s", out)
	}
	// The whole point: the base URL's backing variable is not a credential.
	if strings.Contains(out, "ZAI_BASE_URL") {
		t.Errorf("output offers ZAI_BASE_URL as a credential source; it backs upstream_env.base_url and writing the key to it destroys routing:\n%s", out)
	}
	// The operator must learn that setting a value does not apply it.
	for _, want := range []string{"does not apply", "gc restart"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not state the restart requirement (%q missing):\n%s", want, out)
		}
	}
}

// TestProviderCredentialsSetWritesOnlyTheCredentialVar proves the write half:
// the new value lands on the credential's source variable and nowhere near the
// base URL's, and the file the supervisor reads parses back to it.
func TestProviderCredentialsSetWritesOnlyTheCredentialVar(t *testing.T) {
	cityPath, secretsPath := newCredentialsTestEnv(t)
	if err := os.WriteFile(secretsPath, []byte("# operator notes\nZAI_BASE_URL=https://api.z.ai/api/anthropic\nOTHER_KEY=keepme\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdin, err := os.CreateTemp(t.TempDir(), "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.WriteString("sk-ant-rotated\n"); err != nil {
		t.Fatal(err)
	}
	stdin.Close() //nolint:errcheck

	var stdout, stderr bytes.Buffer
	code := run([]string{"provider", "credentials", "--city", cityPath, "--set-from-file", stdin.Name(), "zai"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d; want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	data, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := processenv.ParseEnvFile(string(data))
	if err != nil {
		t.Fatalf("secrets file does not parse: %v\n%s", err, data)
	}
	if entries["ANTHROPIC_AUTH_TOKEN_ZAI"] != "sk-ant-rotated" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN_ZAI = %q; want sk-ant-rotated", entries["ANTHROPIC_AUTH_TOKEN_ZAI"])
	}
	if got := entries["ZAI_BASE_URL"]; got != "https://api.z.ai/api/anthropic" {
		t.Errorf("ZAI_BASE_URL = %q; want the untouched endpoint — this is the corruption the redesign exists to prevent", got)
	}
	if entries["OTHER_KEY"] != "keepme" {
		t.Errorf("OTHER_KEY = %q; an unrelated credential was lost", entries["OTHER_KEY"])
	}
	if !strings.Contains(string(data), "# operator notes") {
		t.Errorf("operator comment was dropped:\n%s", data)
	}

	// The command must not imply the fleet picked this up.
	if !strings.Contains(stdout.String(), "gc restart") {
		t.Errorf("output does not tell the operator what still has to happen:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "sk-ant-rotated") || strings.Contains(stderr.String(), "sk-ant-rotated") {
		t.Errorf("the credential was echoed back to the terminal:\n%s\n%s", stdout.String(), stderr.String())
	}
}

// TestProviderCredentialsRefusesStaticLiteral covers a provider whose
// credential is inlined in config. There is no variable to write, and silently
// doing nothing at exit 0 is the failure mode the withdrawn command had.
func TestProviderCredentialsRefusesStaticLiteral(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	cityPath := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.inline]
base = "builtin:claude"
env = {ANTHROPIC_AUTH_TOKEN = "sk-ant-inlined", ANTHROPIC_API_KEY = "sk-ant-inlined"}
`)

	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("sk-ant-new"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"provider", "credentials", "--city", cityPath, "--set-from-file", keyFile, "inline"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run() = 0; want non-zero — nothing could be rotated\nstdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "literal") {
		t.Errorf("stderr does not explain that the credential is inlined in config: %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(gcHome, "secrets.env")); err == nil {
		t.Error("a secrets file was written despite the refusal")
	}
}

func TestProviderCredentialsUnknownProvider(t *testing.T) {
	cityPath, _ := newCredentialsTestEnv(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "nope"}, &stdout, &stderr); code == 0 {
		t.Fatalf("run() = 0; want non-zero for an unknown provider\nstdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "nope") {
		t.Errorf("stderr does not name the provider: %q", stderr.String())
	}
}

// TestProviderCredentialsWarnsWhenShellShadowsFile pins the silent-failure
// mode the operator cannot otherwise see: the supervisor's service file is
// rebuilt from the calling shell's environment FIRST, and the secrets file
// only fills keys that scan left unset. A file edit made from a shell that
// still exports the old value has no effect and reports no error.
func TestProviderCredentialsWarnsWhenShellShadowsFile(t *testing.T) {
	cityPath, _ := newCredentialsTestEnv(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN_ZAI", "sk-ant-old-from-shell")

	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("sk-ant-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "--set-from-file", keyFile, "zai"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d; want 0\nstderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "ANTHROPIC_AUTH_TOKEN_ZAI") {
		t.Errorf("no warning that the shell value shadows the file write:\n%s", out)
	}
}

// TestProviderCredentialsRejectsEmptyCredential: an empty stdin is almost
// always a failed `pass show` or a typo in a pipeline. Writing it would blank
// the credential and take the fleet down at the next restart.
func TestProviderCredentialsRejectsEmptyCredential(t *testing.T) {
	cityPath, secretsPath := newCredentialsTestEnv(t)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "credentials", "--city", cityPath, "--set-from-file", keyFile, "zai"}, &stdout, &stderr); code == 0 {
		t.Fatal("run() = 0; want non-zero for an empty credential")
	}
	if !strings.Contains(stderr.String(), "empty") {
		t.Errorf("stderr does not say the credential was empty: %q", stderr.String())
	}
	if _, err := os.Stat(secretsPath); err == nil {
		t.Error("a secrets file was written for an empty credential")
	}
}

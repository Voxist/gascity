package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// TestMigrateJournalFileMatchesPinnedBeads reads the name out of the pinned
// beads module rather than restating it.
//
// The previous spelling of this constant was a plausible-looking guess
// ("migrate-dolt-mode.json"). Nothing failed: the product's post-migration
// verification stat'd a file that could never exist, so it always passed, and so
// did the tests asserting the residue was gone. A constant that is only ever
// compared against itself proves nothing, so this one is compared against bd.
//
// It resolves the module directory from go.mod plus the module cache, and it
// resolves the cache the way the go command does rather than by reading the
// GOMODCACHE variable. Reading the variable silently disabled the comparison:
// `go test` does not export GOMODCACHE into the test process, so the test
// skipped under every plain `go test` — an editor runner, a hook, `go test
// ./...` — and ran only because make's TEST_ENV happens to forward it. A
// contract check that is honest under one wrapper is not a contract check.
//
// Neither does it shell out to `go env`: a subprocess here grows the source
// resource census (verified — the untagged subprocess baseline rejects it) and
// that ratchet is shrink-only, so the resolution is inlined instead.
func TestMigrateJournalFileMatchesPinnedBeads(t *testing.T) {
	modCache := goModuleCache(t)
	path, version := linkedBeadsModule(t)
	escaped, err := module.EscapePath(path)
	if err != nil {
		t.Fatalf("escape module path %q: %v", path, err)
	}
	source, err := os.ReadFile(filepath.Join(modCache, escaped+"@"+version, "cmd", "bd", "migrate_dolt_mode.go"))
	if err != nil {
		t.Skipf("pinned beads %s@%s is not unpacked in the local module cache: %v", path, version, err)
	}

	const decl = `migrateJournalFileName = "`
	idx := strings.Index(string(source), decl)
	if idx < 0 {
		t.Fatalf("pinned beads %s no longer declares migrateJournalFileName; re-derive %s by hand", version, MigrateDoltModeJournalFile)
	}
	rest := string(source)[idx+len(decl):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("could not parse migrateJournalFileName out of the pinned beads source")
	}
	if got := rest[:end]; got != MigrateDoltModeJournalFile {
		t.Fatalf("MigrateDoltModeJournalFile = %q, pinned bd writes %q", MigrateDoltModeJournalFile, got)
	}
}

// goModuleCache resolves the module cache the way the go command does.
//
// Mirrors cmd/go/internal/cfg: the process environment first, then the `go env
// -w` config file ($GOENV, else os.UserConfigDir()/go/env, and "off" means no
// file), then the default $GOPATH/pkg/mod with GOPATH resolved by the same two
// steps and defaulting to $HOME/go. Only the first element of a GOPATH list
// holds the module cache. Every step is a file or an environment read, so the
// test carries no subprocess.
func goModuleCache(t *testing.T) string {
	t.Helper()
	if dir := goEnvValue("GOMODCACHE"); dir != "" {
		return dir
	}
	gopath := goEnvValue("GOPATH")
	if gopath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("resolve home directory to default GOPATH: %v", err)
		}
		gopath = filepath.Join(home, "go")
	}
	roots := filepath.SplitList(gopath)
	if len(roots) == 0 || roots[0] == "" {
		t.Fatalf("GOPATH %q has no usable first element", gopath)
	}
	return filepath.Join(roots[0], "pkg", "mod")
}

// goEnvValue reads one go environment variable: process environment, then the
// `go env -w` config file.
func goEnvValue(name string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return goEnvFileValue(name)
}

// goEnvFileValue reads one key out of the go env config file.
func goEnvFileValue(name string) string {
	path := strings.TrimSpace(os.Getenv("GOENV"))
	switch path {
	case "off":
		return ""
	case "", "auto":
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(dir, "go", "env")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key == name {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// linkedBeadsModule returns the module path and version the go command links
// for github.com/steveyegge/beads: the requirement, or the replacement when
// go.mod replaces it (the bd library bridge, deps.env BD_LIB_BRIDGE_REF). The
// module cache holds a replaced module under the replacement's path, so
// reading the requirement alone would look in a directory the go command never
// filled, and this check would skip on every clean cache.
func linkedBeadsModule(t *testing.T) (string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	path, version, err := linkedBeadsModuleFromGoMod(data)
	if err != nil {
		t.Fatalf("go.mod: %v", err)
	}
	return path, version
}

// linkedBeadsModuleFromGoMod is linkedBeadsModule's resolution, on go.mod text.
func linkedBeadsModuleFromGoMod(data []byte) (string, string, error) {
	const beadsPath = "github.com/steveyegge/beads"
	file, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return "", "", err
	}
	version := ""
	for _, req := range file.Require {
		if req.Mod.Path == beadsPath {
			version = req.Mod.Version
		}
	}
	if version == "" {
		return "", "", fmt.Errorf("does not require %s", beadsPath)
	}
	for _, rep := range file.Replace {
		if rep.Old.Path != beadsPath || (rep.Old.Version != "" && rep.Old.Version != version) {
			continue
		}
		if rep.New.Version == "" {
			return "", "", fmt.Errorf("replaces %s with the local path %q, which has no module cache directory", beadsPath, rep.New.Path)
		}
		return rep.New.Path, rep.New.Version, nil
	}
	return beadsPath, version, nil
}

// TestLinkedBeadsModuleFromGoMod pins the resolution the contract check above
// depends on: under the library bridge it must look in the replacement's
// module cache directory, or it skips on every clean cache.
func TestLinkedBeadsModuleFromGoMod(t *testing.T) {
	const req = "module m\n\ngo 1.24\n\nrequire github.com/steveyegge/beads v1.3.1-0.20261104055431-3c205e0a41e4\n"
	cases := []struct {
		name, goMod, path, version string
		wantErr                    bool
	}{
		{
			name: "require only", goMod: req,
			path: "github.com/steveyegge/beads", version: "v1.3.1-0.20261104055431-3c205e0a41e4",
		},
		{
			name: "bridge replace", goMod: req + "replace github.com/steveyegge/beads => github.com/Voxist/beads v1.1.1-0.20261113233511-b0548453ae8d\n",
			path: "github.com/Voxist/beads", version: "v1.1.1-0.20261113233511-b0548453ae8d",
		},
		{
			name: "replace block", goMod: req + "replace (\n\tgithub.com/steveyegge/beads => github.com/Voxist/beads v1.1.1-0.20261113233511-b0548453ae8d\n)\n",
			path: "github.com/Voxist/beads", version: "v1.1.1-0.20261113233511-b0548453ae8d",
		},
		{
			name: "replace of another version does not apply", goMod: req + "replace github.com/steveyegge/beads v1.0.0 => github.com/Voxist/beads v1.1.1-0.20261113233511-b0548453ae8d\n",
			path: "github.com/steveyegge/beads", version: "v1.3.1-0.20261104055431-3c205e0a41e4",
		},
		{
			name: "replace of another module does not apply", goMod: req + "replace github.com/other/mod => github.com/Voxist/beads v1.1.1-0.20261113233511-b0548453ae8d\n",
			path: "github.com/steveyegge/beads", version: "v1.3.1-0.20261104055431-3c205e0a41e4",
		},
		{name: "local path replace", goMod: req + "replace github.com/steveyegge/beads => ../beads\n", wantErr: true},
		{name: "no requirement", goMod: "module m\n\ngo 1.24\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, version, err := linkedBeadsModuleFromGoMod([]byte(tc.goMod))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %s@%s, want an error", path, version)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if path != tc.path || version != tc.version {
				t.Fatalf("got %s@%s, want %s@%s", path, version, tc.path, tc.version)
			}
		})
	}
}

// TestGoModuleCacheResolutionOrder pins the precedence the skip regression turned
// on. A plain `go test` exports no GOMODCACHE, so if the config file and the
// GOPATH default are not reachable the contract check above goes quiet again.
func TestGoModuleCacheResolutionOrder(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	contents := "GOFLAGS=-mod=mod\nGOMODCACHE=/from/config/file\nGOPATH=/from/config/gopath\n"
	if err := os.WriteFile(envFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("write go env file: %v", err)
	}

	t.Run("environment wins", func(t *testing.T) {
		t.Setenv("GOENV", envFile)
		t.Setenv("GOMODCACHE", "/from/environment")
		if got := goModuleCache(t); got != "/from/environment" {
			t.Fatalf("goModuleCache() = %q, want /from/environment", got)
		}
	})

	t.Run("config file when the environment is silent", func(t *testing.T) {
		t.Setenv("GOENV", envFile)
		t.Setenv("GOMODCACHE", "")
		if got := goModuleCache(t); got != "/from/config/file" {
			t.Fatalf("goModuleCache() = %q, want /from/config/file", got)
		}
	})

	t.Run("GOPATH default when neither names the cache", func(t *testing.T) {
		t.Setenv("GOENV", "off")
		t.Setenv("GOMODCACHE", "")
		t.Setenv("GOPATH", "/from/gopath"+string(os.PathListSeparator)+"/ignored")
		want := filepath.Join("/from/gopath", "pkg", "mod")
		if got := goModuleCache(t); got != want {
			t.Fatalf("goModuleCache() = %q, want %q", got, want)
		}
	})
}

// repositoryRoot walks up from the package directory to the module root.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
			t.Fatalf("GC_TEST_REPO_ROOT=%s has no go.mod: %v", root, err)
		}
		return root
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}

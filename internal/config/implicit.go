package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const implicitImportSchema = 1

// ImplicitImport describes a legacy user-global import record retained for
// compatibility tooling. Config composition no longer splices these imports
// into every city.
type ImplicitImport struct {
	Source  string `toml:"source"`
	Version string `toml:"version"`
	Commit  string `toml:"commit"`
}

type implicitImportFile struct {
	Schema  int                       `toml:"schema"`
	Imports map[string]ImplicitImport `toml:"imports"`
}

// ReadImplicitImports reads ~/.gc/implicit-import.toml (or $GC_HOME) and
// returns its imports. Missing files are treated as empty.
func ReadImplicitImports() (map[string]ImplicitImport, string, error) {
	imports, path, _, err := readImplicitImportsWithData()
	return imports, path, err
}

func readImplicitImportsWithData() (map[string]ImplicitImport, string, []byte, error) {
	path := implicitImportPath()
	if path == "" {
		return map[string]ImplicitImport{}, "", nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]ImplicitImport{}, path, nil, nil
		}
		return nil, path, nil, fmt.Errorf("reading implicit imports: %w", err)
	}

	var file implicitImportFile
	if _, err := toml.Decode(string(data), &file); err != nil {
		return nil, path, nil, fmt.Errorf("parsing implicit imports: %w", err)
	}
	if file.Schema != 0 && file.Schema != implicitImportSchema {
		return nil, path, nil, fmt.Errorf("unsupported implicit import schema %d", file.Schema)
	}
	if file.Imports == nil {
		file.Imports = make(map[string]ImplicitImport)
	}
	return file.Imports, path, data, nil
}

func implicitImportPath() string {
	home := ImplicitGCHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "implicit-import.toml")
}

// ImplicitGCHome returns the user-global GC_HOME directory used to
// resolve implicit-import bookkeeping and bootstrap pack caches.
//
// Resolution order: GC_HOME env var → user home/.gc → tmp fallback.
// Returns "" under `go test` to keep unit tests hermetic unless the
// caller opts in by setting GC_HOME explicitly.
func ImplicitGCHome() string {
	if v := strings.TrimSpace(os.Getenv("GC_HOME")); v != "" {
		return v
	}
	// Keep unit tests hermetic unless they explicitly opt into a GC_HOME.
	if strings.HasSuffix(os.Args[0], ".test") {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".gc")
	}
	return filepath.Join(home, ".gc")
}

// repoCacheBaseDir resolves the user-global GC base directory for the shared
// pack-cache root: GC_HOME env var → user home/.gc → tmp fallback.
//
// Unlike ImplicitGCHome it never returns "" under `go test`: the pack cache
// must always resolve to a usable directory so production behavior is
// byte-identical to the pre-helper computation (prod sets GC_HOME, or falls
// back to ~/.gc — the same path either way). Hermeticity in tests is achieved
// by setting GC_HOME explicitly (e.g. t.Setenv("GC_HOME", t.TempDir())), which
// this helper honors.
func repoCacheBaseDir() string {
	if v := strings.TrimSpace(os.Getenv("GC_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".gc")
	}
	return filepath.Join(home, ".gc")
}

// RepoCacheRoot returns the shared, machine-local pack-cache root
// (<base>/cache/repos), where <base> is resolved by repoCacheBaseDir:
// GC_HOME env var → user home/.gc → tmp fallback.
//
// This is the single canonical computation of the repo cache root. All call
// sites must use it (directly or via packman.RepoCacheRoot, which delegates
// here) so that tests setting GC_HOME isolate the cache instead of polluting
// the developer's real ~/.gc/cache/repos.
func RepoCacheRoot() string {
	return filepath.Join(repoCacheBaseDir(), "cache", "repos")
}

// GlobalRepoCachePath returns the user-global cache path for a source+commit pair.
func GlobalRepoCachePath(gcHome, source, commit string) string {
	return filepath.Join(gcHome, "cache", "repos", GlobalRepoCacheDirName(source, commit))
}

// GlobalRepoCacheDirName returns the user-global cache directory name for a source+commit pair.
func GlobalRepoCacheDirName(source, commit string) string {
	return RepoCacheKey(source, commit)
}

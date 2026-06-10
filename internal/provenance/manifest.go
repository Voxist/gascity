// Package provenance records and verifies deploy provenance for installed
// binaries. `make install` writes a machine-derived build manifest (commit
// SHA, source repo path, build time) next to the installed binary; doctor
// checks compare the running binary's embedded VCS revision against that
// manifest and assert the manifest commit is part of the source repo's
// lineage. The manifest is machine-derived, never human-maintained: a
// "running == on-disk" comparison alone passes when both are stale, so the
// lineage assertion is what actually catches stale deploys.
package provenance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ManifestSchemaVersion is the current build-manifest schema version.
const ManifestSchemaVersion = "1"

// ManifestSuffix is appended to an installed binary's path to derive its
// build-manifest path (e.g. ~/go/bin/gc -> ~/go/bin/gc.buildinfo.json).
const ManifestSuffix = ".buildinfo.json"

// Manifest records what a `make install` deployed: the exact commit the
// binary was built from, where the source repo lives, and when the build
// happened. All fields are machine-derived at install time.
type Manifest struct {
	// SchemaVersion identifies the manifest format; see ManifestSchemaVersion.
	SchemaVersion string `json:"schema_version"`
	// CommitSHA is the full git commit hash the binary was built from.
	CommitSHA string `json:"commit_sha"`
	// SourceRepo is the absolute path of the source repository the binary
	// was built in. Doctor lineage checks run read-only git commands here.
	SourceRepo string `json:"source_repo"`
	// BuildTime is when the manifest was written (UTC at install time).
	BuildTime time.Time `json:"build_time"`
}

// ManifestPath returns the build-manifest path for an installed binary.
func ManifestPath(binaryPath string) string {
	return binaryPath + ManifestSuffix
}

// Write atomically persists m to path (temp file + rename). It rejects
// manifests without a commit SHA: an unverifiable manifest is worse than a
// missing one because it looks authoritative.
func Write(path string, m Manifest) error {
	if strings.TrimSpace(m.CommitSHA) == "" {
		return fmt.Errorf("writing build manifest %q: commit SHA is empty", path)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding build manifest %q: %w", path, err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file for build manifest %q: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()        //nolint:errcheck // best-effort cleanup
		os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("writing build manifest %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("closing build manifest %q: %w", path, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("setting build manifest mode %q: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("renaming build manifest into place %q: %w", path, err)
	}
	return nil
}

// Load reads and validates the build manifest at path. Missing files
// surface fs.ErrNotExist via the wrapped error so callers can degrade
// gracefully when no manifest was ever installed.
func Load(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("reading build manifest %q: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parsing build manifest %q: %w", path, err)
	}
	if strings.TrimSpace(m.CommitSHA) == "" {
		return Manifest{}, fmt.Errorf("build manifest %q has no commit SHA", path)
	}
	return m, nil
}

// Capture derives a manifest from the git repository at repoPath: the
// commit SHA is `git rev-parse HEAD` (read-only) and the build time is the
// supplied timestamp. The repo path is recorded verbatim so doctor lineage
// checks know where to run read-only git queries later.
func Capture(repoPath string, buildTime time.Time) (Manifest, error) {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return Manifest{}, fmt.Errorf("resolving HEAD of %q: %w", repoPath, err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return Manifest{}, fmt.Errorf("resolving HEAD of %q: empty revision", repoPath)
	}
	return Manifest{
		SchemaVersion: ManifestSchemaVersion,
		CommitSHA:     sha,
		SourceRepo:    repoPath,
		BuildTime:     buildTime,
	}, nil
}

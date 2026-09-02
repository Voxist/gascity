package main

import (
	"fmt"
	"io"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// beadsModulePath is the linked beads library module; three deployed gc
// binaries once linked three different versions of it while all reporting
// the same gc version string, so it is first-class version output now.
const beadsModulePath = "github.com/steveyegge/beads"

// Build metadata — injected via ldflags at build time.
// Falls back to VCS info embedded by the Go toolchain (go install, go build).
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
	// buildBase is the fork-base lineage stamp (e.g.
	// "Voxist/main@eb743642c+0-0") injected by `make artifact`; empty for
	// builds that never proved their lineage.
	buildBase                = ""
	beadsVersion             = "unknown"
	goPseudoVersionSuffixRes = []*regexp.Regexp{
		regexp.MustCompile(`^(.*)\.0\.\d{14}-[0-9a-f]{12,}(?:\+\S*)?$`),
		regexp.MustCompile(`^(.*)-0\.\d{14}-[0-9a-f]{12,}(?:\+\S*)?$`),
		regexp.MustCompile(`^(.*)-\d{14}-[0-9a-f]{12,}(?:\+\S*)?$`),
	}
)

const (
	// dirtySuffix marks a commit built from a modified working tree. It is
	// part of the build identity consumers compare: the supervisor reports it
	// as /health build_id and binary-drift detection matches on it verbatim.
	dirtySuffix = "-dirty"
	// minAbbrevCommitLen is the shortest abbreviation trusted to identify a
	// commit, matching git's own default abbreviation floor. Anything shorter
	// collides too easily to prove two stamps describe the same revision.
	minAbbrevCommitLen = 7
)

func init() {
	info, ok := debug.ReadBuildInfo()
	version, commit, date = resolveBuildMetadata(version, commit, date, ok, info)
	beadsVersion = resolveBeadsVersion(ok, info)
}

// resolveBeadsVersion reports the effective linked beads library version
// from the embedded module info, honoring replace directives (a replaced
// module is what the binary actually runs; a local-path replace has no
// version, so the path itself is the most honest answer).
func resolveBeadsVersion(ok bool, info *debug.BuildInfo) string {
	if !ok || info == nil {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep == nil || dep.Path != beadsModulePath {
			continue
		}
		mod := dep
		if dep.Replace != nil {
			mod = dep.Replace
		}
		if mod.Version != "" {
			return mod.Version
		}
		return mod.Path
	}
	return "unknown"
}

func resolveBuildMetadata(
	currentVersion string,
	currentCommit string,
	currentDate string,
	ok bool,
	info *debug.BuildInfo,
) (string, string, string) {
	currentVersion = normalizeVersion(currentVersion)
	if !ok || info == nil {
		return currentVersion, currentCommit, currentDate
	}
	if currentVersion == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		currentVersion = normalizeVersion(info.Main.Version)
	}
	var dirty bool
	var revision string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
			if currentCommit == "unknown" && s.Value != "" {
				currentCommit = s.Value
			}
		case "vcs.time":
			if currentDate == "unknown" && s.Value != "" {
				currentDate = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty && currentCommit != "unknown" &&
		!strings.HasSuffix(currentCommit, dirtySuffix) &&
		vcsDescribesCommit(revision, currentCommit) {
		currentCommit += dirtySuffix
	}
	return currentVersion, currentCommit, currentDate
}

// vcsDescribesCommit reports whether the Go toolchain's embedded vcs.revision
// describes the same commit this binary was linked against, and so whether its
// companion vcs.modified flag says anything about *our* working tree.
//
// The toolchain locates a repository by looking for a `.git` directory. Inside
// a git worktree `.git` is a gitdir file, which it does not recognize, so it
// walks up the filesystem and stamps whichever repository encloses the
// worktree. Where worktrees are nested inside another checkout — a polecat
// worktree under the city directory, for instance — the embedded revision and
// dirtiness belong to that unrelated repository, and a pristine tree gets
// reported as `<our-commit>-dirty` (ga-u7fb). Comparing the two stamps catches
// exactly that case: an ldflags-injected commit that the embedded revision
// does not describe means the VCS metadata came from somewhere else.
//
// The stamps legitimately differ in length — the build injects an abbreviated
// hash while the toolchain records the full one — so either being a prefix of
// the other counts as a match.
func vcsDescribesCommit(revision, injectedCommit string) bool {
	if revision == "" {
		// No revision recorded to contradict the flag. Nothing to check
		// against, so the toolchain keeps the benefit of the doubt.
		return true
	}
	shorter := strings.ToLower(strings.TrimSuffix(injectedCommit, dirtySuffix))
	longer := strings.ToLower(revision)
	if shorter == longer {
		// Identical stamps, including the case where the commit was adopted
		// from this very revision. No independent claims to reconcile.
		return true
	}
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	// Two independent stamps: the shorter must be a long enough abbreviation
	// to actually identify the commit before a prefix match proves anything.
	if len(shorter) < minAbbrevCommitLen {
		return false
	}
	return strings.HasPrefix(longer, shorter)
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" || v == "(devel)" {
		return "dev"
	}
	// Strip +incompatible only (Go v2+ module compat sentinel for repos without a /vN import path).
	// Preserve all other build metadata (e.g. +ra.1 marks a locally-patched release).
	v = strings.TrimSuffix(v, "+incompatible")
	for _, re := range goPseudoVersionSuffixRes {
		if m := re.FindStringSubmatch(v); len(m) == 2 {
			v = m[1]
			break
		}
	}
	if v == "" || v == "0.0.0" {
		return "dev"
	}
	return v
}

func newVersionCmd(stdout, stderr io.Writer) *cobra.Command {
	var longOutput bool
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print gc version",
		Long: `Print the gc version string.

Use --long to include git commit, build date, linked beads library, and
build-base lineage metadata (base is "unstamped" for builds not produced
via 'make artifact').`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			base := buildBase
			if base == "" {
				base = "unstamped"
			}
			if jsonOut {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc version", versionJSONResult{
					SchemaVersion: "1",
					Version:       version,
					Commit:        commit,
					Date:          date,
					BeadsVersion:  beadsVersion,
					BuildBase:     base,
					Long:          longOutput,
				})
			}
			if longOutput {
				fmt.Fprintf(stdout, "%s\n", formatLongVersion(version, commit, date, beadsVersion, buildBase)) //nolint:errcheck // best-effort stdout
				return nil
			}
			fmt.Fprintf(stdout, "%s\n", version) //nolint:errcheck // best-effort stdout
			return nil
		},
	}
	cmd.Flags().BoolVarP(&longOutput, "long", "l", false, "Include git commit and build date metadata")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	return cmd
}

// formatLongVersion renders the --long output. An empty base renders as
// "unstamped" — provenance silence must be visible, not blank.
func formatLongVersion(version, commit, date, beads, base string) string {
	if base == "" {
		base = "unstamped"
	}
	return fmt.Sprintf("%s (commit: %s, built: %s, beads: %s, base: %s)", version, commit, date, beads, base)
}

type versionJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	Date          string `json:"date"`
	BeadsVersion  string `json:"beads_version"`
	BuildBase     string `json:"build_base"`
	Long          bool   `json:"long"`
}

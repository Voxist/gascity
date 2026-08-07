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
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
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
	if dirty && currentCommit != "unknown" {
		currentCommit += "-dirty"
	}
	return currentVersion, currentCommit, currentDate
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

// Command writebuildmanifest writes the deploy-provenance build manifest
// next to an installed binary. `make install` invokes it after copying the
// binary into place so the manifest (commit SHA + source repo path + build
// time) is machine-derived from the repo that produced the build, never
// human-maintained. The `deploy-provenance` doctor check consumes it.
//
// Usage:
//
//	writebuildmanifest -binary /path/to/installed/gc [-repo /path/to/source]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/provenance"
)

func main() {
	binary := flag.String("binary", "", "path of the installed binary; the manifest is written next to it (required)")
	repo := flag.String("repo", ".", "path of the source git repository the binary was built from")
	flag.Parse()

	if err := run(*binary, *repo, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "writebuildmanifest: %v\n", err) //nolint:errcheck // best-effort stderr
		os.Exit(1)
	}
}

func run(binary, repo string, stdout *os.File) error {
	if binary == "" {
		return fmt.Errorf("-binary is required")
	}
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return fmt.Errorf("resolving repo path %q: %w", repo, err)
	}
	m, err := provenance.Capture(repoAbs, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("capturing build manifest: %w", err)
	}
	path := provenance.ManifestPath(binary)
	if err := provenance.Write(path, m); err != nil {
		return fmt.Errorf("writing build manifest: %w", err)
	}
	fmt.Fprintf(stdout, "Wrote %s (commit %s)\n", path, m.CommitSHA) //nolint:errcheck // best-effort stdout
	return nil
}

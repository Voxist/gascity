// Command artifactname derives the provenance-correct name and base-lineage
// stamp for a gc release artifact. `make artifact` invokes it before
// building so the artifact filename is machine-derived from the ACTUAL
// build commit (git rev-parse HEAD) and the base-branch token is only used
// when HEAD really is in the base's lineage — never from a base/merge ref a
// human happened to have in mind.
//
// The base must be a remote-tracking ref (e.g. Voxist/main): in this repo
// `origin` is the upstream, so an unqualified "main" claim is exactly the
// wrong-remote trap this tool exists to refuse.
//
// Usage:
//
//	artifactname -base <remote>/<branch> [-repo dir] [-binary gc] [-allow-dirty] [-format eval|name]
//
// -format eval (default) prints POSIX-shell assignments for eval in a
// Makefile recipe:
//
//	ARTIFACT_NAME='gc-main-20260716-eb743642c'
//	ARTIFACT_HEAD_SHA='eb743642c...'
//	ARTIFACT_COMMIT_STAMP='eb743642c...'          (gains -dirty when the tree is dirty)
//	ARTIFACT_BASE_STAMP='Voxist/main@eb743642c+0-0'
//
// -format name prints just the artifact filename.
//
// All facts come from `git -C <repo>` queries, never from the Go
// toolchain's buildvcs stamping — which, from a linked worktree nested
// under the repo directory, records the MAIN checkout's HEAD and dirty
// state instead of the worktree's (and records nothing from a worktree
// outside it). Artifact builds therefore pass -buildvcs=false and inject
// ARTIFACT_COMMIT_STAMP via -X main.commit.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/provenance"
)

func main() {
	repo := flag.String("repo", ".", "path of the git repository being built")
	base := flag.String("base", "", "remote-tracking base ref the lineage claim is made against, e.g. Voxist/main (required)")
	binary := flag.String("binary", "gc", "binary name prefix for the artifact")
	allowDirty := flag.Bool("allow-dirty", false, "permit a dirty working tree; the artifact name gains a -dirty suffix instead of failing")
	format := flag.String("format", "eval", "output format: eval (shell assignments) or name (filename only)")
	flag.Parse()

	if err := run(*repo, *base, *binary, *allowDirty, *format, time.Now().UTC(), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "artifactname: %v\n", err) //nolint:errcheck // best-effort stderr
		os.Exit(1)
	}
}

func run(repo, base, binary string, allowDirty bool, format string, now time.Time, stdout *os.File) error {
	if base == "" {
		return fmt.Errorf("-base is required (e.g. -base Voxist/main); the lineage claim must name the remote")
	}
	a, err := provenance.DeriveArtifact(repo, base)
	if err != nil {
		return err
	}
	if a.Dirty && !allowDirty {
		return fmt.Errorf("working tree of %q is dirty: a binary built now would not correspond to any commit; commit first, or pass -allow-dirty to get an explicit -dirty name", repo)
	}
	name := a.Name(binary, now)
	switch format {
	case "name":
		fmt.Fprintf(stdout, "%s\n", name) //nolint:errcheck // best-effort stdout
	case "eval":
		fmt.Fprintf(stdout, "ARTIFACT_NAME=%s\n", provenance.ShellSingleQuote(name))                    //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "ARTIFACT_HEAD_SHA=%s\n", provenance.ShellSingleQuote(a.HeadSHA))           //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "ARTIFACT_COMMIT_STAMP=%s\n", provenance.ShellSingleQuote(a.CommitStamp())) //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "ARTIFACT_BASE_STAMP=%s\n", provenance.ShellSingleQuote(a.BaseStamp()))     //nolint:errcheck // best-effort stdout
	default:
		return fmt.Errorf("unknown -format %q (want eval or name)", format)
	}
	return nil
}

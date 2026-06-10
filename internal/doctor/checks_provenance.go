package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/gastownhall/gascity/internal/provenance"
)

// DefaultLineageRef is the git ref in the source repo whose head the
// deployed commit must be an ancestor of (or equal to). City-Scale plan
// item 1.4: deploys are built from the fork integration branch, so a
// binary whose commit is not reachable from fork/main HEAD is either
// stale or built from a stray branch — incident-5's actual shape.
const DefaultLineageRef = "fork/main"

// DeployProvenanceCheck asserts machine-derived deploy provenance:
//  1. the running binary's embedded vcs.revision matches the build
//     manifest `make install` wrote next to the installed binary, and
//  2. that manifest commit is ancestor-or-equal of the source repo's
//     lineage ref head (`git merge-base --is-ancestor`, read-only).
//
// A plain "running == on-disk" comparison passes when both are stale, so
// the lineage assertion is the load-bearing half. The check degrades to a
// warning (never a hard failure) when provenance simply cannot be
// asserted: no manifest installed, no embedded revision (test binaries),
// or the source repo/ref being unavailable on this machine.
type DeployProvenanceCheck struct {
	// BinaryPath resolves the on-disk path of the running binary. Defaults
	// to os.Executable with symlinks resolved.
	BinaryPath func() (string, error)
	// RunningRevision reports the VCS revision embedded in the running
	// binary. Defaults to debug.ReadBuildInfo's vcs.revision setting.
	RunningRevision func() (string, bool)
	// LineageRef is the source-repo ref the deployed commit must be
	// reachable from. Defaults to DefaultLineageRef.
	LineageRef string
}

// NewDeployProvenanceCheck returns a DeployProvenanceCheck with the
// default self-inspection resolvers and lineage ref.
func NewDeployProvenanceCheck() *DeployProvenanceCheck {
	return &DeployProvenanceCheck{
		BinaryPath:      runningBinaryPath,
		RunningRevision: runningBinaryRevision,
		LineageRef:      DefaultLineageRef,
	}
}

// Name returns the check identifier.
func (c *DeployProvenanceCheck) Name() string { return "deploy-provenance" }

// Run executes the provenance assertions described on the type.
func (c *DeployProvenanceCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	binary, err := c.BinaryPath()
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("cannot resolve running binary path; provenance not asserted: %v", err)
		return r
	}
	manifestPath := provenance.ManifestPath(binary)
	manifest, err := provenance.Load(manifestPath)
	if errors.Is(err, fs.ErrNotExist) {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("no build manifest at %s; provenance not asserted", manifestPath)
		r.FixHint = "run `make install` from the source repo to record deploy provenance"
		return r
	}
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("build manifest unreadable: %v", err)
		r.FixHint = "re-run `make install` from the source repo to rewrite the manifest"
		return r
	}

	revision, ok := c.RunningRevision()
	if !ok || strings.TrimSpace(revision) == "" {
		r.Status = StatusWarning
		r.Message = "running binary embeds no vcs.revision; provenance not asserted"
		return r
	}
	if !revisionsEqual(revision, manifest.CommitSHA) {
		r.Status = StatusError
		r.Message = fmt.Sprintf("running binary revision %s does not match installed manifest %s (stale process or clobbered binary)",
			shortRevision(revision), shortRevision(manifest.CommitSHA))
		r.FixHint = "restart the supervisor/city so the freshly installed binary is the one running, or re-run `make install`"
		return r
	}

	ref := c.LineageRef
	if strings.TrimSpace(ref) == "" {
		ref = DefaultLineageRef
	}
	repo := manifest.SourceRepo
	if info, statErr := os.Stat(repo); repo == "" || statErr != nil || !info.IsDir() {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("source repo %q unavailable; lineage not asserted", repo)
		return r
	}
	if !gitRefResolves(repo, ref) {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("lineage ref %q not found in %s; lineage not asserted", ref, repo)
		return r
	}
	ancestor, err := gitIsAncestor(repo, manifest.CommitSHA, ref)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("lineage could not be verified: %v", err)
		return r
	}
	if !ancestor {
		r.Status = StatusError
		r.Message = fmt.Sprintf("deployed commit %s is not an ancestor of %s in %s (built from a stray or rebased-away branch)",
			shortRevision(manifest.CommitSHA), ref, repo)
		r.FixHint = fmt.Sprintf("rebuild and reinstall from %s of %s", ref, repo)
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("running binary %s matches the install manifest and is ancestor-or-equal of %s", shortRevision(manifest.CommitSHA), ref)
	return r
}

// CanFix returns false — remediation is a rebuild/reinstall, not something
// the doctor should perform.
func (c *DeployProvenanceCheck) CanFix() bool { return false }

// Fix is a no-op; the check is not auto-fixable.
func (c *DeployProvenanceCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *DeployProvenanceCheck) WarmupEligible() bool { return false }

// Cheap opts this check into the supervisor-cadence subset: it reads one
// small file and runs at most two read-only local git commands.
func (c *DeployProvenanceCheck) Cheap() bool { return true }

// runningBinaryPath resolves the running binary's on-disk path with
// symlinks resolved, so the manifest lookup lands next to the real file
// `make install` wrote (e.g. through the ~/.local/bin compatibility link).
func runningBinaryPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving running executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolving executable symlinks for %q: %w", p, err)
	}
	return resolved, nil
}

// runningBinaryRevision returns the vcs.revision embedded by the Go
// toolchain in the running binary, if any.
func runningBinaryRevision() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return "", false
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value, true
		}
	}
	return "", false
}

// revisionsEqual compares two git revisions, tolerating one being a short
// form of the other (ldflags-injected commits are short; vcs.revision is
// full). Prefix matches require at least the conventional 7 characters.
func revisionsEqual(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(a) < 7 || len(b) < 7 {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// shortRevision abbreviates a revision for display.
func shortRevision(rev string) string {
	rev = strings.TrimSpace(rev)
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// gitRefResolves reports whether ref names a commit in repo (read-only).
func gitRefResolves(repo, ref string) bool {
	return exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run() == nil
}

// gitIsAncestor reports whether commit is ancestor-or-equal of ref in repo
// via `git merge-base --is-ancestor` (read-only). Exit status 1 means "not
// an ancestor"; any other failure is returned as an error so callers can
// degrade instead of misreporting.
func gitIsAncestor(repo, commit, ref string) (bool, error) {
	cmd := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", commit, ref)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	msg := strings.TrimSpace(string(out))
	if msg != "" {
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s in %q: %s: %w", shortRevision(commit), ref, repo, msg, err)
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s in %q: %w", shortRevision(commit), ref, repo, err)
}

// BeadsExpectedBuildCheck compares `bd --version` output against the
// `[beads] expected_build` pin. It catches the brew-clobber class: a
// package upgrade or wrong-branch rebuild silently replacing a custom bd
// build the city depends on. An empty pin disables the comparison.
type BeadsExpectedBuildCheck struct {
	// Expected is the configured `[beads] expected_build` token that must
	// appear in `bd --version` output. Empty disables the check.
	Expected string
	// VersionOutput runs `bd --version` and returns its combined output.
	// Injectable for tests.
	VersionOutput func() (string, error)
}

// NewBeadsExpectedBuildCheck returns a BeadsExpectedBuildCheck for the
// configured expected-build token with the default `bd --version` runner.
func NewBeadsExpectedBuildCheck(expected string) *BeadsExpectedBuildCheck {
	return &BeadsExpectedBuildCheck{
		Expected: expected,
		VersionOutput: func() (string, error) {
			out, err := exec.Command("bd", "--version").CombinedOutput()
			return string(out), err
		},
	}
}

// Name returns the check identifier.
func (c *BeadsExpectedBuildCheck) Name() string { return "beads-expected-build" }

// Run compares `bd --version` output against the configured pin.
func (c *BeadsExpectedBuildCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	expected := strings.TrimSpace(c.Expected)
	if expected == "" {
		r.Status = StatusOK
		r.Message = "no [beads] expected_build configured; bd build not pinned"
		return r
	}
	out, err := c.VersionOutput()
	version := strings.TrimSpace(out)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("[beads] expected_build = %q but `bd --version` failed: %v", expected, err)
		if version != "" {
			r.Details = append(r.Details, version)
		}
		r.FixHint = "install the pinned bd build (or fix PATH) so `bd --version` succeeds"
		return r
	}
	if !strings.Contains(version, expected) {
		r.Status = StatusError
		r.Message = fmt.Sprintf("bd build mismatch: `bd --version` = %q does not contain expected_build %q", version, expected)
		r.FixHint = "reinstall the pinned bd build, or update [beads] expected_build if the new build is intentional"
		return r
	}
	r.Status = StatusOK
	r.Message = fmt.Sprintf("bd build matches expected_build %q", expected)
	return r
}

// CanFix returns false — the right bd build must be installed by the
// operator's deploy procedure, not by the doctor.
func (c *BeadsExpectedBuildCheck) CanFix() bool { return false }

// Fix is a no-op; the check is not auto-fixable.
func (c *BeadsExpectedBuildCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *BeadsExpectedBuildCheck) WarmupEligible() bool { return false }

// Cheap opts this check into the supervisor-cadence subset: one short
// subprocess that never touches a store.
func (c *BeadsExpectedBuildCheck) Cheap() bool { return true }

// bdContextProbeDetailLimit caps how many probe-output lines are attached
// to the check result.
const bdContextProbeDetailLimit = 6

// BdContextProbeCheck is the post-install contract probe: `bd context`
// must resolve from the city root. This is the gc-hook dead-drop /
// SEC-003 signature — when bd cannot resolve its context where the
// controller shells out, hooks silently return no work. The probe is a
// non-fatal advisory warning because some cities legitimately have no
// beads context at the city root (e.g. file-backed stores).
type BdContextProbeCheck struct {
	// Probe runs `bd context` with the given working directory and returns
	// its combined output. Injectable for tests.
	Probe func(dir string) (string, error)
}

// NewBdContextProbeCheck returns a BdContextProbeCheck with the default
// `bd context` runner.
func NewBdContextProbeCheck() *BdContextProbeCheck {
	return &BdContextProbeCheck{
		Probe: func(dir string) (string, error) {
			cmd := exec.Command("bd", "context")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			return string(out), err
		},
	}
}

// Name returns the check identifier.
func (c *BdContextProbeCheck) Name() string { return "bd-context-probe" }

// Run probes `bd context` from the city root.
func (c *BdContextProbeCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	cityPath := ""
	if ctx != nil {
		cityPath = ctx.CityPath
	}
	if strings.TrimSpace(cityPath) == "" {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = "no city path available; bd context not probed"
		return r
	}
	out, err := c.Probe(cityPath)
	if err != nil {
		r.Status = StatusWarning
		r.Severity = SeverityAdvisory
		r.Message = fmt.Sprintf("`bd context` did not resolve from the city root: %v", err)
		r.Details = probeOutputDetails(out)
		r.FixHint = "if this city uses a bd-backed store, fix the .beads context (see engdocs gc-hook dead-drop notes); otherwise ignore"
		return r
	}
	r.Status = StatusOK
	r.Message = "`bd context` resolves from the city root"
	return r
}

// CanFix returns false — context remediation is store-specific and
// operator-owned.
func (c *BdContextProbeCheck) CanFix() bool { return false }

// Fix is a no-op; the check is not auto-fixable.
func (c *BdContextProbeCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this check is not part of the
// `gc start` warm-up scan.
func (c *BdContextProbeCheck) WarmupEligible() bool { return false }

// probeOutputDetails trims probe output into a bounded detail list.
func probeOutputDetails(out string) []string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	details := make([]string, 0, bdContextProbeDetailLimit)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(details) == bdContextProbeDetailLimit {
			details = append(details, "…")
			break
		}
		details = append(details, line)
	}
	return details
}

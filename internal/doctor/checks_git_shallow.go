package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// GitShallowCheck warns when any git repository in the workspace — the city
// root itself or any configured rig clone, suspended rigs included — is a
// shallow clone. One depth-limited `git fetch`/`git clone` grafts
// .git/shallow and silently poisons every later git analysis in that clone:
// merge-base returns nothing and the history looks truncated.
//
// Incident record: voxist-platform's "re-rooted main" false alarm (a shared
// clone shallowed by `git fetch --depth=1`, analyzed 2026-07-28) was exactly
// this; a 2026-09-28 census across the fleet found voxist-web-stack
// (shallowed Jul 16) and asr-medical (shallowed Sep 22) still poisoned — the
// actor is an agent economizing a fetch, and it recurs. Detection is a pure
// `git rev-parse --is-shallow-repository` per repository, which opens no bead
// store — the reason the per-rig checks skip suspended rigs (bd auto-start of
// orphan Dolt servers, ga-wzk) does not apply, and both poisoned rigs were
// suspended, so coverage must not skip them either.
type GitShallowCheck struct {
	cityPath string
	targets  []gitShallowTarget
	gitPath  func(name string) (string, error) // injectable for tests
}

type gitShallowTarget struct {
	label string
	path  string
}

// NewGitShallowCheckForConfig creates a shallow-clone check covering the city
// root and every configured rig. With a broken config (cfgErr != nil) the
// city root stays covered — a broken city.toml is exactly when silent
// corruption is most likely, mirroring the worktree checks' contract.
func NewGitShallowCheckForConfig(cityPath string, cfg *config.City, cfgErr error) *GitShallowCheck {
	c := &GitShallowCheck{cityPath: cityPath, gitPath: exec.LookPath}
	c.targets = append(c.targets, gitShallowTarget{label: "city root", path: cityPath})
	if cfgErr != nil || cfg == nil {
		return c
	}
	for _, rig := range cfg.Rigs {
		rigPath := strings.TrimSpace(rig.Path)
		if rigPath == "" {
			continue
		}
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		c.targets = append(c.targets, gitShallowTarget{label: "rig " + rig.Name, path: rigPath})
	}
	return c
}

// Name returns the check identifier.
func (c *GitShallowCheck) Name() string { return "git-shallow" }

// WarmupEligible returns true so this check runs during gc start warm-up.
func (c *GitShallowCheck) WarmupEligible() bool { return true }

// CanFix returns false; unshallowing is a potentially large network fetch an
// operator should run deliberately (the FixHint carries the exact command).
func (c *GitShallowCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *GitShallowCheck) Fix(_ *CheckContext) error { return nil }

// Run rev-parse --is-shallow-repository on every target and warn on any
// shallow graft. Missing paths and non-git directories are skipped silently —
// rig:path and rig:git own those verdicts. A rev-parse failure on a directory
// that does have a .git entry warns (fail-closed): unknown must be
// distinguishable from healthy.
func (c *GitShallowCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}

	gitBin, err := c.gitPath("git")
	if err != nil {
		r.Status = StatusWarning
		r.Message = "unable to determine shallow state — git unavailable"
		return r
	}

	var (
		shallowLabels []string
		shallowDetail []string
		undetermined  []string
		fixPaths      []string
		checked       int
	)
	for _, target := range c.targets {
		if fi, err := os.Stat(target.path); err != nil || !fi.IsDir() {
			continue
		}
		out, err := runGitCommand(gitBin, target.path, "rev-parse", "--is-shallow-repository")
		if err != nil {
			if _, statErr := os.Stat(filepath.Join(target.path, ".git")); statErr != nil {
				continue
			}
			undetermined = append(undetermined,
				fmt.Sprintf("%s (%s): unable to determine shallow state: %v", target.label, target.path, err))
			continue
		}
		checked++
		if strings.TrimSpace(out) != "true" {
			continue
		}
		shallowLabels = append(shallowLabels, target.label)
		entry := fmt.Sprintf("%s (%s) is shallow — .git/shallow grafts history", target.label, target.path)
		if fi, err := os.Stat(filepath.Join(target.path, ".git", "shallow")); err == nil {
			entry += fmt.Sprintf(" (graft mtime %s)", fi.ModTime().UTC().Format(time.RFC3339))
		}
		shallowDetail = append(shallowDetail, entry)
		fixPaths = append(fixPaths, target.path)
	}

	if len(shallowLabels) > 0 {
		noun := "repositories are"
		if len(shallowLabels) == 1 {
			noun = "repository is"
		}
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("%d git %s shallow (depth-grafted by a fetch --depth / clone --depth) — git history analysis in %s is silently wrong (vp-vj42)",
			len(shallowLabels), noun, strings.Join(shallowLabels, ", "))
		r.Details = append(r.Details, shallowDetail...)
		r.Details = append(r.Details, undetermined...)
		r.FixHint = fmt.Sprintf("git -C %q fetch --unshallow", fixPaths[0])
		return r
	}
	if len(undetermined) > 0 {
		r.Status = StatusWarning
		r.Message = "unable to determine shallow state for some repositories"
		r.Details = undetermined
		return r
	}
	r.Status = StatusOK
	r.Message = fmt.Sprintf("git-shallow: %d repositories checked, none shallow", checked)
	return r
}

package doctor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// RED tests for GitShallowCheck (vp-vj42). They fail to compile until the
// check lands in internal/doctor/checks_git_shallow.go.
//
// Incident shape being guarded: a depth-limited `git fetch`/`git clone`
// against a shared clone grafts .git/shallow and silently poisons every
// later git analysis in that clone — merge-base returns nothing and the
// history looks truncated. voxist-platform's "re-rooted main" false alarm
// (vp-vj42, 2026-07-28) was exactly this; a 2026-09-28 census found
// voxist-web-stack (shallowed Jul 16) and asr-medical (shallowed Sep 22)
// still poisoned. Unlike the per-rig checks, this check must cover
// suspended rigs too — both poisoned rigs were suspended, and detection
// is a pure `git rev-parse --is-shallow-repository` that opens no bead
// store (the reason per-rig checks skip suspended rigs, ga-wzk, does not
// apply here).

func TestGitShallowCheck_ShallowRigWarns(t *testing.T) {
	cityPath := initGitRepoForShallowTest(t)
	sick := cloneRepoForShallowTest(t, cityPath, true)
	c := NewGitShallowCheckForConfig(cityPath, &config.City{
		Rigs: []config.Rig{{Name: "sick", Path: sick}},
	}, nil)

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning for a shallow rig clone", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("severity = %d, want SeverityAdvisory", r.Severity)
	}
	if !strings.Contains(r.Message, "sick") {
		t.Errorf("message = %q, want the rig name named", r.Message)
	}
	foundPath := false
	for _, detail := range r.Details {
		if strings.Contains(detail, sick) {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("Details = %v, want the shallow clone's path", r.Details)
	}
	if r.FixHint == "" || !strings.Contains(r.FixHint, "fetch --unshallow") {
		t.Errorf("FixHint = %q, want a git fetch --unshallow hint", r.FixHint)
	}
}

func TestGitShallowCheck_AllHealthyOK(t *testing.T) {
	cityPath := initGitRepoForShallowTest(t)
	healthy := cloneRepoForShallowTest(t, cityPath, false)
	c := NewGitShallowCheckForConfig(cityPath, &config.City{
		Rigs: []config.Rig{{Name: "healthy", Path: healthy}},
	}, nil)

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK when no repo is shallow", r.Status, r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q, want empty for OK result", r.FixHint)
	}
}

func TestGitShallowCheck_MissingAndNonGitRigsSkipped(t *testing.T) {
	cityPath := initGitRepoForShallowTest(t)
	empty := t.TempDir() // exists but is not a git repository
	c := NewGitShallowCheckForConfig(cityPath, &config.City{
		Rigs: []config.Rig{
			{Name: "ghost", Path: filepath.Join(t.TempDir(), "does-not-exist")},
			{Name: "notgit", Path: empty},
		},
	}, nil)

	r := c.Run(&CheckContext{})

	// Rig path and git-repo verdicts belong to rig:path / rig:git; this check
	// stays silent on them rather than triple-warning the same rig.
	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK — missing and non-git rigs are other checks' verdicts", r.Status, r.Message)
	}
}

func TestGitShallowCheck_SuspendedRigCovered(t *testing.T) {
	cityPath := initGitRepoForShallowTest(t)
	suspended := cloneRepoForShallowTest(t, cityPath, true)
	c := NewGitShallowCheckForConfig(cityPath, &config.City{
		Rigs: []config.Rig{{Name: "asleep", Path: suspended, Suspended: true}},
	}, nil)

	r := c.Run(&CheckContext{})

	// The differentiator: per-rig checks skip suspended rigs (opening their
	// bead store auto-starts orphan Dolt servers, ga-wzk). This check opens
	// no store, so a suspended rig that has been depth-grafted must still
	// warn — both rigs found poisoned on 2026-09-28 were suspended.
	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning — suspended rigs must stay covered", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "asleep") {
		t.Errorf("message = %q, want the suspended rig named", r.Message)
	}
}

func TestGitShallowCheck_CityRootShallowWarns(t *testing.T) {
	cityPath := cloneRepoForShallowTest(t, initGitRepoForShallowTest(t), true)
	healthy := cloneRepoForShallowTest(t, initGitRepoForShallowTest(t), false)
	c := NewGitShallowCheckForConfig(cityPath, &config.City{
		Rigs: []config.Rig{{Name: "healthy", Path: healthy}},
	}, nil)

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning for a shallow city root", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "city root") {
		t.Errorf("message = %q, want the city root named", r.Message)
	}
}

func TestGitShallowCheck_ConfigErrorStillChecksCityRoot(t *testing.T) {
	// Mirrors the worktree checks' contract: a broken city.toml is exactly
	// when silent corruption is most likely, so the city root stays covered
	// even when the config could not be parsed (cfg = nil, cfgErr != nil).
	cityPath := cloneRepoForShallowTest(t, initGitRepoForShallowTest(t), true)
	c := NewGitShallowCheckForConfig(cityPath, nil, errors.New("city.toml: parse error"))

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning — cfgErr must not skip the city root", r.Status, r.Message)
	}
}

func TestGitShallowCheck_GitUnavailableWarnsNotSilentOK(t *testing.T) {
	cityPath := initGitRepoForShallowTest(t)
	healthy := cloneRepoForShallowTest(t, cityPath, false)
	c := NewGitShallowCheckForConfig(cityPath, &config.City{
		Rigs: []config.Rig{{Name: "healthy", Path: healthy}},
	}, nil)
	c.gitPath = func(string) (string, error) {
		return "", errors.New("git unavailable")
	}

	r := c.Run(&CheckContext{})

	// Fail-closed: an unreadable shallow state must read as a warning, never
	// as a clean bill of health — unknown and healthy must be distinguishable.
	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning when git is unavailable", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "unable to determine") {
		t.Errorf("message = %q, want an unable-to-determine warning", r.Message)
	}
}

func TestGitShallowCheck_ShallowDetailCarriesGraftEvidence(t *testing.T) {
	cityPath := initGitRepoForShallowTest(t)
	sick := cloneRepoForShallowTest(t, cityPath, true)
	c := NewGitShallowCheckForConfig(cityPath, &config.City{
		Rigs: []config.Rig{{Name: "sick", Path: sick}},
	}, nil)

	r := c.Run(&CheckContext{})

	found := false
	for _, detail := range r.Details {
		if strings.Contains(detail, ".git/shallow") {
			found = true
		}
	}
	if !found {
		t.Errorf("Details = %v, want a detail naming .git/shallow as the graft evidence", r.Details)
	}
}

// initGitRepoForShallowTest returns a normal (unshallowed) git repository to
// serve as a city root or clone source.
func initGitRepoForShallowTest(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := t.TempDir()
	runGitForShallowTest(t, dir, "init")
	runGitForShallowTest(t, dir, "checkout", "-b", "main")
	runGitForShallowTest(t, dir, "config", "user.name", "Git Shallow Test")
	runGitForShallowTest(t, dir, "config", "user.email", "git-shallow@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	runGitForShallowTest(t, dir, "add", "README.md")
	runGitForShallowTest(t, dir, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(dir, "SECOND.md"), []byte("second\n"), 0o600); err != nil {
		t.Fatalf("write second file: %v", err)
	}
	runGitForShallowTest(t, dir, "add", "SECOND.md")
	runGitForShallowTest(t, dir, "commit", "-m", "second")
	return dir
}

// cloneRepoForShallowTest clones src, shallowly when shallow is set. A local
// path silently ignores --depth, so the file:// transport is required for the
// graft to actually land.
func cloneRepoForShallowTest(t *testing.T, src string, shallow bool) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "clone")
	url := "file://" + src
	args := []string{"clone"}
	if shallow {
		args = append(args, "--depth=1")
	}
	args = append(args, url, dst)
	runGitForShallowTest(t, filepath.Dir(dst), args...)
	if shallow {
		if _, err := os.Stat(filepath.Join(dst, ".git", "shallow")); err != nil {
			t.Fatalf("expected %s/.git/shallow after --depth=1 clone: %v", dst, err)
		}
	}
	return dst
}

func runGitForShallowTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

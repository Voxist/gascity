package doctor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/provenance"
)

// provenanceTestRepo builds a git repo with two commits:
// base -> head on branch fork/main, plus a divergent commit on branch
// stray that is NOT an ancestor of fork/main. Returns the repo path and
// the three SHAs.
func provenanceTestRepo(t *testing.T) (repo, base, head, stray string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	repo = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("config", "user.name", "Provenance Check Test")
	git("config", "user.email", "provenance-check@example.invalid")
	git("checkout", "-b", "fork/main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	git("add", "a.txt")
	git("commit", "-m", "base")
	base = git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o600); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	git("add", "b.txt")
	git("commit", "-m", "head")
	head = git("rev-parse", "HEAD")
	git("checkout", "-b", "stray", base)
	if err := os.WriteFile(filepath.Join(repo, "c.txt"), []byte("c\n"), 0o600); err != nil {
		t.Fatalf("write c.txt: %v", err)
	}
	git("add", "c.txt")
	git("commit", "-m", "stray")
	stray = git("rev-parse", "HEAD")
	git("checkout", "fork/main")
	return repo, base, head, stray
}

// writeProvenanceManifest writes a manifest for a fake installed binary in
// its own temp dir and returns the binary path the check should inspect.
func writeProvenanceManifest(t *testing.T, repo, sha string) string {
	t.Helper()
	binDir := t.TempDir()
	binary := filepath.Join(binDir, "gc")
	m := provenance.Manifest{
		SchemaVersion: provenance.ManifestSchemaVersion,
		CommitSHA:     sha,
		SourceRepo:    repo,
		BuildTime:     time.Now().UTC(),
	}
	if err := provenance.Write(provenance.ManifestPath(binary), m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return binary
}

func newTestDeployProvenanceCheck(binary, revision string, haveRevision bool) *DeployProvenanceCheck {
	c := NewDeployProvenanceCheck()
	c.BinaryPath = func() (string, error) { return binary, nil }
	c.RunningRevision = func() (string, bool) { return revision, haveRevision }
	return c
}

func TestDeployProvenanceCheck(t *testing.T) {
	repo, base, head, stray := provenanceTestRepo(t)

	tests := []struct {
		name        string
		check       *DeployProvenanceCheck
		wantStatus  CheckStatus
		wantMessage string
	}{
		{
			name:        "ancestor commit passes",
			check:       newTestDeployProvenanceCheck(writeProvenanceManifest(t, repo, base), base, true),
			wantStatus:  StatusOK,
			wantMessage: "ancestor",
		},
		{
			name:        "head commit passes",
			check:       newTestDeployProvenanceCheck(writeProvenanceManifest(t, repo, head), head, true),
			wantStatus:  StatusOK,
			wantMessage: "ancestor",
		},
		{
			name:        "running revision differs from manifest",
			check:       newTestDeployProvenanceCheck(writeProvenanceManifest(t, repo, base), head, true),
			wantStatus:  StatusError,
			wantMessage: "does not match",
		},
		{
			name:        "manifest commit not an ancestor of lineage ref",
			check:       newTestDeployProvenanceCheck(writeProvenanceManifest(t, repo, stray), stray, true),
			wantStatus:  StatusError,
			wantMessage: "not an ancestor",
		},
		{
			name:        "missing manifest degrades to warning",
			check:       newTestDeployProvenanceCheck(filepath.Join(t.TempDir(), "gc"), head, true),
			wantStatus:  StatusWarning,
			wantMessage: "no build manifest",
		},
		{
			name:        "missing running revision degrades to warning",
			check:       newTestDeployProvenanceCheck(writeProvenanceManifest(t, repo, head), "", false),
			wantStatus:  StatusWarning,
			wantMessage: "revision",
		},
		{
			name:        "unavailable source repo degrades to warning",
			check:       newTestDeployProvenanceCheck(writeProvenanceManifest(t, filepath.Join(repo, "gone"), head), head, true),
			wantStatus:  StatusWarning,
			wantMessage: "lineage not asserted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.check.Run(&CheckContext{})
			if r.Status != tt.wantStatus {
				t.Fatalf("status = %d (%s), want %d", r.Status, r.Message, tt.wantStatus)
			}
			if !strings.Contains(r.Message, tt.wantMessage) {
				t.Errorf("message = %q, want substring %q", r.Message, tt.wantMessage)
			}
		})
	}
}

func TestDeployProvenanceCheckMissingLineageRef(t *testing.T) {
	repo, _, head, _ := provenanceTestRepo(t)
	c := newTestDeployProvenanceCheck(writeProvenanceManifest(t, repo, head), head, true)
	c.LineageRef = "no/such/ref"
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no/such/ref") {
		t.Errorf("message = %q, want missing ref name", r.Message)
	}
}

func TestDeployProvenanceCheckPrefixRevisionMatches(t *testing.T) {
	repo, _, head, _ := provenanceTestRepo(t)
	// go's vcs.revision is the full SHA; ldflags-injected commits are often
	// short. A 12-char prefix of the manifest SHA must still match.
	c := newTestDeployProvenanceCheck(writeProvenanceManifest(t, repo, head), head[:12], true)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK for prefix revision", r.Status, r.Message)
	}
}

func TestDeployProvenanceCheckDefaults(t *testing.T) {
	c := NewDeployProvenanceCheck()
	if c.Name() != "deploy-provenance" {
		t.Errorf("Name = %q, want deploy-provenance", c.Name())
	}
	if c.BinaryPath == nil || c.RunningRevision == nil {
		t.Error("NewDeployProvenanceCheck must populate default resolvers")
	}
	if c.LineageRef != DefaultLineageRef {
		t.Errorf("LineageRef = %q, want %q", c.LineageRef, DefaultLineageRef)
	}
	if c.CanFix() {
		t.Error("CanFix = true, want false")
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible = true, want false")
	}
}

func TestBeadsExpectedBuildCheck(t *testing.T) {
	tests := []struct {
		name       string
		expected   string
		output     string
		err        error
		wantStatus CheckStatus
	}{
		{
			name:       "unset expected_build is OK",
			expected:   "",
			wantStatus: StatusOK,
		},
		{
			name:       "matching token is OK",
			expected:   "1.0.5-pooling",
			output:     "bd version 1.0.5-pooling (abc1234)",
			wantStatus: StatusOK,
		},
		{
			name:       "mismatched token fails",
			expected:   "1.0.5-pooling",
			output:     "bd version 1.0.4 (brew)",
			wantStatus: StatusError,
		},
		{
			name:       "bd --version failure fails when pinned",
			expected:   "1.0.5-pooling",
			err:        errors.New("exec: bd: not found"),
			wantStatus: StatusError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewBeadsExpectedBuildCheck(tt.expected)
			calls := 0
			c.VersionOutput = func() (string, error) {
				calls++
				return tt.output, tt.err
			}
			r := c.Run(&CheckContext{})
			if r.Status != tt.wantStatus {
				t.Fatalf("status = %d (%s), want %d", r.Status, r.Message, tt.wantStatus)
			}
			if tt.expected == "" && calls != 0 {
				t.Errorf("VersionOutput called %d times for unset pin, want 0", calls)
			}
		})
	}
}

func TestBeadsExpectedBuildCheckDefaults(t *testing.T) {
	c := NewBeadsExpectedBuildCheck("x")
	if c.Name() != "beads-expected-build" {
		t.Errorf("Name = %q, want beads-expected-build", c.Name())
	}
	if c.VersionOutput == nil {
		t.Error("NewBeadsExpectedBuildCheck must populate the default runner")
	}
	if c.CanFix() {
		t.Error("CanFix = true, want false")
	}
}

func TestBdContextProbeCheck(t *testing.T) {
	t.Run("resolving context is OK", func(t *testing.T) {
		c := NewBdContextProbeCheck()
		var gotDir string
		c.Probe = func(dir string) (string, error) {
			gotDir = dir
			return "context: city\n", nil
		}
		r := c.Run(&CheckContext{CityPath: "/some/city"})
		if r.Status != StatusOK {
			t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
		}
		if gotDir != "/some/city" {
			t.Errorf("probe dir = %q, want city root", gotDir)
		}
	})
	t.Run("failure is a non-fatal advisory warning", func(t *testing.T) {
		c := NewBdContextProbeCheck()
		c.Probe = func(string) (string, error) {
			return "Error: no beads context found", errors.New("exit status 1")
		}
		r := c.Run(&CheckContext{CityPath: "/some/city"})
		if r.Status != StatusWarning {
			t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
		}
		if r.Severity != SeverityAdvisory {
			t.Fatalf("severity = %d, want SeverityAdvisory", r.Severity)
		}
		if len(r.Details) == 0 || !strings.Contains(strings.Join(r.Details, "\n"), "no beads context") {
			t.Errorf("details = %v, want probe output", r.Details)
		}
	})
	t.Run("defaults", func(t *testing.T) {
		c := NewBdContextProbeCheck()
		if c.Name() != "bd-context-probe" {
			t.Errorf("Name = %q, want bd-context-probe", c.Name())
		}
		if c.Probe == nil {
			t.Error("NewBdContextProbeCheck must populate the default probe")
		}
		if c.CanFix() {
			t.Error("CanFix = true, want false")
		}
	})
}

func TestCheapChecksSubset(t *testing.T) {
	deploy := NewDeployProvenanceCheck()
	expected := NewBeadsExpectedBuildCheck("x")
	probe := NewBdContextProbeCheck()
	role := &BeadsRoleCheck{}

	if !IsCheap(deploy) {
		t.Error("IsCheap(DeployProvenanceCheck) = false, want true")
	}
	if !IsCheap(expected) {
		t.Error("IsCheap(BeadsExpectedBuildCheck) = false, want true")
	}
	if IsCheap(probe) {
		t.Error("IsCheap(BdContextProbeCheck) = true, want false (spawns bd against live stores)")
	}
	if IsCheap(role) {
		t.Error("IsCheap(BeadsRoleCheck) = true, want false (not opted in)")
	}

	got := CheapChecks([]Check{deploy, probe, expected, role})
	if len(got) != 2 {
		t.Fatalf("CheapChecks returned %d checks, want 2", len(got))
	}
	if got[0] != Check(deploy) || got[1] != Check(expected) {
		t.Errorf("CheapChecks = %v, want [deploy, expected] preserving order", got)
	}
}

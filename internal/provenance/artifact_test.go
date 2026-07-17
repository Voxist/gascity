package provenance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initArtifactTestRepo creates a repo whose HEAD is published as the
// remote-tracking ref voxist/main (no network involved), mirroring the
// fork-remote layout the artifact rules exist for.
func initArtifactTestRepo(t *testing.T) string {
	t.Helper()
	dir := initProvenanceTestRepo(t)
	provenanceTestGit(t, dir, "branch", "-M", "work")
	publishBase(t, dir, "HEAD")
	return dir
}

// publishBase points refs/remotes/voxist/main at rev, standing in for a
// fetched fork remote.
func publishBase(t *testing.T, dir, rev string) {
	t.Helper()
	sha := provenanceTestGitOut(t, dir, "rev-parse", rev)
	provenanceTestGit(t, dir, "update-ref", "refs/remotes/voxist/main", sha)
}

func artifactTestCommit(t *testing.T, dir, msg string) {
	t.Helper()
	provenanceTestGit(t, dir, "commit", "--allow-empty", "-m", msg)
}

func TestDeriveArtifactInBaseLineage(t *testing.T) {
	dir := initArtifactTestRepo(t)
	head := provenanceTestGitOut(t, dir, "rev-parse", "HEAD")

	a, err := DeriveArtifact(dir, "voxist/main")
	if err != nil {
		t.Fatalf("DeriveArtifact: %v", err)
	}
	if a.Token != "main" {
		t.Errorf("Token = %q, want %q", a.Token, "main")
	}
	if a.HeadSHA != head {
		t.Errorf("HeadSHA = %q, want %q", a.HeadSHA, head)
	}
	if len(a.ShortSHA) < 9 || !strings.HasPrefix(head, a.ShortSHA) {
		t.Errorf("ShortSHA = %q, want >=9-char prefix of %q", a.ShortSHA, head)
	}
	if a.Ahead != 0 || a.Behind != 0 {
		t.Errorf("Ahead/Behind = %d/%d, want 0/0", a.Ahead, a.Behind)
	}
	if a.Dirty {
		t.Error("Dirty = true on a clean tree")
	}
	if a.BaseRef != "voxist/main" {
		t.Errorf("BaseRef = %q, want %q", a.BaseRef, "voxist/main")
	}
	if !strings.HasPrefix(head, a.BaseSHA) {
		t.Errorf("BaseSHA = %q, want prefix of %q", a.BaseSHA, head)
	}
}

func TestDeriveArtifactBehindBaseStillMainToken(t *testing.T) {
	// Building an OLD main commit is honest "main" lineage; the staleness
	// must land in Behind, not silently vanish.
	dir := initArtifactTestRepo(t)
	old := provenanceTestGitOut(t, dir, "rev-parse", "HEAD")
	artifactTestCommit(t, dir, "advance base")
	publishBase(t, dir, "HEAD")
	provenanceTestGit(t, dir, "checkout", "--detach", old)

	a, err := DeriveArtifact(dir, "voxist/main")
	if err != nil {
		t.Fatalf("DeriveArtifact: %v", err)
	}
	if a.Token != "main" {
		t.Errorf("Token = %q, want %q", a.Token, "main")
	}
	if a.Ahead != 0 || a.Behind != 1 {
		t.Errorf("Ahead/Behind = %d/%d, want 0/1", a.Ahead, a.Behind)
	}
}

func TestDeriveArtifactSideBranchGetsBranchToken(t *testing.T) {
	dir := initArtifactTestRepo(t)
	provenanceTestGit(t, dir, "checkout", "-b", "fix/order-dispatch_v2")
	artifactTestCommit(t, dir, "side work")

	a, err := DeriveArtifact(dir, "voxist/main")
	if err != nil {
		t.Fatalf("DeriveArtifact: %v", err)
	}
	if a.Token != "fix-order-dispatch_v2" {
		t.Errorf("Token = %q, want %q", a.Token, "fix-order-dispatch_v2")
	}
	if a.Ahead != 1 || a.Behind != 0 {
		t.Errorf("Ahead/Behind = %d/%d, want 1/0", a.Ahead, a.Behind)
	}
}

func TestDeriveArtifactRefusesMisleadingBaseName(t *testing.T) {
	// A local branch NAMED main that is not in the fork main's lineage is
	// exactly the origin-vs-fork trap; the name must be refused, not derived.
	dir := initArtifactTestRepo(t)
	provenanceTestGit(t, dir, "checkout", "-b", "main")
	artifactTestCommit(t, dir, "diverged local main")

	_, err := DeriveArtifact(dir, "voxist/main")
	if err == nil {
		t.Fatal("DeriveArtifact on diverged branch named main: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not an ancestor") {
		t.Errorf("error %q should explain the lineage refusal", err)
	}
}

func TestDeriveArtifactDetachedNotAncestor(t *testing.T) {
	dir := initArtifactTestRepo(t)
	provenanceTestGit(t, dir, "checkout", "--detach")
	artifactTestCommit(t, dir, "detached work")

	a, err := DeriveArtifact(dir, "voxist/main")
	if err != nil {
		t.Fatalf("DeriveArtifact: %v", err)
	}
	if a.Token != "detached" {
		t.Errorf("Token = %q, want %q", a.Token, "detached")
	}
}

func TestDeriveArtifactDirty(t *testing.T) {
	dir := initArtifactTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatalf("dirty tracked file: %v", err)
	}
	a, err := DeriveArtifact(dir, "voxist/main")
	if err != nil {
		t.Fatalf("DeriveArtifact: %v", err)
	}
	if !a.Dirty {
		t.Error("Dirty = false with modified tracked file")
	}
}

func TestDeriveArtifactUntrackedCountsAsDirty(t *testing.T) {
	// Go's own vcs.modified counts untracked files; the name must agree
	// with what the binary will embed.
	dir := initArtifactTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatalf("untracked file: %v", err)
	}
	a, err := DeriveArtifact(dir, "voxist/main")
	if err != nil {
		t.Fatalf("DeriveArtifact: %v", err)
	}
	if !a.Dirty {
		t.Error("Dirty = false with untracked file present")
	}
}

func TestDeriveArtifactRequiresRemoteTrackingBase(t *testing.T) {
	dir := initArtifactTestRepo(t)
	// "work" exists as a local branch; a lineage claim against it names no
	// remote and must be rejected.
	if _, err := DeriveArtifact(dir, "work"); err == nil {
		t.Fatal("DeriveArtifact with local-branch base: want error, got nil")
	}
	if _, err := DeriveArtifact(dir, "nosuch/main"); err == nil {
		t.Fatal("DeriveArtifact with unknown base: want error, got nil")
	}
}

func TestArtifactName(t *testing.T) {
	a := Artifact{Token: "main", ShortSHA: "77916fc6c"}
	date := time.Date(2026, 7, 10, 23, 59, 0, 0, time.UTC)
	if got, want := a.Name("gc", date), "gc-main-20260710-77916fc6c"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	a.Dirty = true
	if got, want := a.Name("gc", date), "gc-main-20260710-77916fc6c-dirty"; got != want {
		t.Errorf("Name (dirty) = %q, want %q", got, want)
	}
}

func TestArtifactCommitStamp(t *testing.T) {
	a := Artifact{HeadSHA: "eb743642c6b4935e07dc864a96e7003195dc123a"}
	if got, want := a.CommitStamp(), "eb743642c6b4935e07dc864a96e7003195dc123a"; got != want {
		t.Errorf("CommitStamp = %q, want %q", got, want)
	}
	a.Dirty = true
	if got, want := a.CommitStamp(), "eb743642c6b4935e07dc864a96e7003195dc123a-dirty"; got != want {
		t.Errorf("CommitStamp (dirty) = %q, want %q", got, want)
	}
}

func TestArtifactBaseStamp(t *testing.T) {
	a := Artifact{BaseRef: "Voxist/main", BaseSHA: "eb743642c", Ahead: 1, Behind: 340}
	if got, want := a.BaseStamp(), "Voxist/main@eb743642c+1-340"; got != want {
		t.Errorf("BaseStamp = %q, want %q", got, want)
	}
}

func TestShellSingleQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{in: "plain", want: "'plain'"},
		{in: "with space", want: "'with space'"},
		{in: "don't", want: `'don'\''t'`},
	}
	for _, tt := range tests {
		if got := ShellSingleQuote(tt.in); got != tt.want {
			t.Errorf("ShellSingleQuote(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

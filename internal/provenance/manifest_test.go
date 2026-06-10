package provenance

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManifestPath(t *testing.T) {
	got := ManifestPath("/usr/local/bin/gc")
	want := "/usr/local/bin/gc" + ManifestSuffix
	if got != want {
		t.Errorf("ManifestPath = %q, want %q", got, want)
	}
}

func TestWriteLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gc"+ManifestSuffix)
	when := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	m := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		CommitSHA:     "0123456789abcdef0123456789abcdef01234567",
		SourceRepo:    "/data/projects/gascity",
		BuildTime:     when,
	}
	if err := Write(path, m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != m {
		t.Errorf("round trip = %+v, want %+v", got, m)
	}
}

func TestWriteRejectsEmptyCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gc"+ManifestSuffix)
	err := Write(path, Manifest{SourceRepo: dir})
	if err == nil {
		t.Fatal("Write with empty CommitSHA: want error, got nil")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("Write with empty CommitSHA left a file behind: stat err = %v", statErr)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent"+ManifestSuffix))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load missing file: err = %v, want fs.ErrNotExist", err)
	}
}

func TestLoadRejectsEmptyCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gc"+ManifestSuffix)
	if err := os.WriteFile(path, []byte(`{"schema_version":"1","commit_sha":"","source_repo":"x"}`), 0o644); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load with empty commit_sha: want error, got nil")
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gc"+ManifestSuffix)
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load with malformed JSON: want error, got nil")
	}
}

func TestCaptureFromGitRepo(t *testing.T) {
	repo := initProvenanceTestRepo(t)
	head := provenanceTestGitOut(t, repo, "rev-parse", "HEAD")
	when := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	m, err := Capture(repo, when)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if m.CommitSHA != head {
		t.Errorf("CommitSHA = %q, want %q", m.CommitSHA, head)
	}
	if m.SourceRepo != repo {
		t.Errorf("SourceRepo = %q, want %q", m.SourceRepo, repo)
	}
	if !m.BuildTime.Equal(when) {
		t.Errorf("BuildTime = %v, want %v", m.BuildTime, when)
	}
	if m.SchemaVersion != ManifestSchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", m.SchemaVersion, ManifestSchemaVersion)
	}
}

func TestCaptureNonRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if _, err := Capture(t.TempDir(), time.Now()); err == nil {
		t.Error("Capture on a non-repo: want error, got nil")
	}
}

func initProvenanceTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := t.TempDir()
	provenanceTestGit(t, dir, "init")
	provenanceTestGit(t, dir, "config", "user.name", "Provenance Test")
	provenanceTestGit(t, dir, "config", "user.email", "provenance@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	provenanceTestGit(t, dir, "add", "README.md")
	provenanceTestGit(t, dir, "commit", "-m", "initial")
	return dir
}

func provenanceTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func provenanceTestGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

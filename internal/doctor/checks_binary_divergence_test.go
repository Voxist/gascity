package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFakeBinary writes an executable file with the given content and
// returns its path.
func writeFakeBinary(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// symlink creates newname -> oldname and returns newname.
func symlink(t *testing.T, oldname, newname string) string {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Fatalf("symlinking %s -> %s: %v", newname, oldname, err)
	}
	return newname
}

// divergenceCheck builds a check with both discovery routes stubbed, so the
// test controls exactly which two paths are compared.
func divergenceCheck(pid int, running, verified string) *BinaryDivergenceCheck {
	return &BinaryDivergenceCheck{
		supervisorPID:       pid,
		goos:                "testos",
		resolveRunningImage: func(int) (string, error) { return running, nil },
		lookPath:            func(string) (string, error) { return verified, nil },
		versionOf:           func(p string) string { return "version-of-" + filepath.Base(p) },
	}
}

func TestBinaryDivergenceCheck_SameBinary(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeBinary(t, filepath.Join(dir, "gc"), "the one artifact")

	r := divergenceCheck(4242, bin, bin).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, "same binary") {
		t.Errorf("Message = %q, want mention of the same binary", r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q, want empty on a passing check", r.FixHint)
	}
}

// TestBinaryDivergenceCheck_SymlinksToOneArtifact covers the healthy end state
// of vc-lwif: several PATH entries all pointing at one dated artifact. A false
// positive here would fire on every correctly converged deployment.
func TestBinaryDivergenceCheck_SymlinksToOneArtifact(t *testing.T) {
	dir := t.TempDir()
	artifact := writeFakeBinary(t, filepath.Join(dir, "gc-main-20260904-bc13ce43a"), "the one artifact")
	viaOnePath := symlink(t, artifact, filepath.Join(dir, "gc-a"))
	viaAnotherPath := symlink(t, artifact, filepath.Join(dir, "gc-b"))

	r := divergenceCheck(4242, viaOnePath, viaAnotherPath).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for two symlinks to one artifact; message=%q details=%v",
			r.Status, r.Message, r.Details)
	}
	resolved, err := filepath.EvalSymlinks(artifact)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if !strings.Contains(r.Message, resolved) {
		t.Errorf("Message = %q, want it to name the shared artifact %s", r.Message, resolved)
	}
	joined := strings.Join(r.Details, "\n")
	for _, want := range []string{viaOnePath, viaAnotherPath} {
		if !strings.Contains(joined, want) {
			t.Errorf("Details missing %s: %v", want, r.Details)
		}
	}
}

// TestBinaryDivergenceCheck_HardLinkedToOneArtifact covers the other healthy
// convergence shape: two names for one inode, which EvalSymlinks cannot
// collapse because neither name is a symlink.
func TestBinaryDivergenceCheck_HardLinkedToOneArtifact(t *testing.T) {
	dir := t.TempDir()
	artifact := writeFakeBinary(t, filepath.Join(dir, "gc-artifact"), "the one artifact")
	linked := filepath.Join(dir, "gc-linked")
	if err := os.Link(artifact, linked); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}

	r := divergenceCheck(4242, artifact, linked).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for two links to one inode; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "same file") {
		t.Errorf("Message = %q, want mention that the two paths are the same file", r.Message)
	}
}

// TestBinaryDivergenceCheck_IdenticalContentDistinctFiles covers two real
// copies of one build. Different inodes, same bytes: the fleet runs what the
// probe verified, so this must not warn.
func TestBinaryDivergenceCheck_IdenticalContentDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	a := writeFakeBinary(t, filepath.Join(dir, "gc-a"), "identical bytes")
	b := writeFakeBinary(t, filepath.Join(dir, "gc-b"), "identical bytes")

	r := divergenceCheck(4242, a, b).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for identical content; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "identical content") {
		t.Errorf("Message = %q, want mention of identical content", r.Message)
	}
}

// TestBinaryDivergenceCheck_DifferentBinaries is the incident itself: the
// supervisor executing one build while every probe hits another.
func TestBinaryDivergenceCheck_DifferentBinaries(t *testing.T) {
	dir := t.TempDir()
	executed := writeFakeBinary(t, filepath.Join(dir, "release-gc"), "the build the fleet runs")
	verified := writeFakeBinary(t, filepath.Join(dir, "dev-gc"), "a different, newer build")

	// The verified binary is three weeks newer than the executed one, as in
	// the incident: probes were verifying ahead of the fleet.
	old := time.Now().Add(-21 * 24 * time.Hour)
	if err := os.Chtimes(executed, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	r := divergenceCheck(28611, executed, verified).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	// Both paths must be named — an operator cannot act on "they differ".
	for _, want := range []string{executed, verified} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("Message = %q, want it to name %s", r.Message, want)
		}
	}
	if !strings.Contains(r.Message, "28611") {
		t.Errorf("Message = %q, want it to name the supervisor pid", r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	// Both versions and both modification times must be reported.
	for _, want := range []string{
		"version-of-release-gc",
		"version-of-dev-gc",
		"modified=",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Details missing %q: %v", want, r.Details)
		}
	}
	// Direction is the operationally load-bearing part, so it must survive
	// without --verbose: the message, not the details.
	if !strings.Contains(r.Message, "verified binary is NEWER") {
		t.Errorf("Message does not report which side is newer: %q", r.Message)
	}
	if !strings.Contains(r.Message, "ahead of the running fleet") {
		t.Errorf("Message does not say the probe is ahead of the fleet: %q", r.Message)
	}
	if r.FixHint == "" {
		t.Error("FixHint is empty; a divergent result must tell the operator what to do")
	}
}

// TestBinaryDivergenceCheck_DifferentBinariesExecutedNewer asserts the other
// direction reads correctly: the fleet ahead of the probe.
func TestBinaryDivergenceCheck_DifferentBinariesExecutedNewer(t *testing.T) {
	dir := t.TempDir()
	executed := writeFakeBinary(t, filepath.Join(dir, "new-gc"), "the newer build the fleet runs")
	verified := writeFakeBinary(t, filepath.Join(dir, "old-gc"), "an older build on PATH")

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(verified, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	r := divergenceCheck(1, executed, verified).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning", r.Status)
	}
	if !strings.Contains(r.Message, "executed binary is NEWER") {
		t.Errorf("Message does not report the executed binary as newer: %q", r.Message)
	}
	if !strings.Contains(r.Message, "behind the running fleet") {
		t.Errorf("Message does not say the probe is behind the fleet: %q", r.Message)
	}
}

// TestBinaryDivergenceCheck_SupervisorNotRunning asserts no false alarm — and
// that neither discovery route is even consulted.
func TestBinaryDivergenceCheck_SupervisorNotRunning(t *testing.T) {
	c := &BinaryDivergenceCheck{
		supervisorPID: 0,
		goos:          "testos",
		resolveRunningImage: func(int) (string, error) {
			t.Error("resolveRunningImage called with no supervisor running")
			return "", nil
		},
		lookPath: func(string) (string, error) {
			t.Error("lookPath called with no supervisor running")
			return "", nil
		},
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "supervisor not running") {
		t.Errorf("Message = %q, want mention that the supervisor is not running", r.Message)
	}
}

// TestBinaryDivergenceCheck_UnsupportedPlatform asserts the check says it did
// not run rather than silently passing.
func TestBinaryDivergenceCheck_UnsupportedPlatform(t *testing.T) {
	c := &BinaryDivergenceCheck{
		supervisorPID:       999,
		goos:                "plan9",
		resolveRunningImage: nil,
		lookPath: func(string) (string, error) {
			t.Error("lookPath called on a platform with no image resolver")
			return "", nil
		},
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK", r.Status)
	}
	if !strings.Contains(r.Message, "NOT checked") || !strings.Contains(r.Message, "plan9") {
		t.Errorf("Message = %q, want it to name the platform and say the check did not run", r.Message)
	}
}

func TestBinaryDivergenceCheck_ResolveError(t *testing.T) {
	c := divergenceCheck(77, "", "")
	c.resolveRunningImage = func(int) (string, error) { return "", fmt.Errorf("operation not permitted") }

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "operation not permitted") {
		t.Errorf("Message = %q, want the underlying error", r.Message)
	}
}

// TestBinaryDivergenceCheck_DeletedImage covers a binary replaced in place
// under a running supervisor: nothing on disk describes the running bytes.
func TestBinaryDivergenceCheck_DeletedImage(t *testing.T) {
	c := divergenceCheck(31337, "/opt/gc/bin/gc-old"+deletedImageSuffix, "/usr/local/bin/gc")

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "deleted image") {
		t.Errorf("Message = %q, want mention of a deleted image", r.Message)
	}
	if !strings.Contains(r.Message, "/opt/gc/bin/gc-old") || strings.Contains(r.Message, deletedImageSuffix) {
		t.Errorf("Message = %q, want the unlinked path without the kernel's suffix", r.Message)
	}
}

func TestBinaryDivergenceCheck_NotOnPath(t *testing.T) {
	dir := t.TempDir()
	executed := writeFakeBinary(t, filepath.Join(dir, "gc"), "running")
	c := divergenceCheck(5, executed, "")
	c.lookPath = func(string) (string, error) { return "", fmt.Errorf("executable file not found in $PATH") }

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not on PATH") || !strings.Contains(r.Message, executed) {
		t.Errorf("Message = %q, want it to note the missing PATH entry and name the executed image", r.Message)
	}
}

func TestBinaryDivergenceCheck_Metadata(t *testing.T) {
	c := NewBinaryDivergenceCheck(0)
	if c.Name() != "binary-divergence" {
		t.Errorf("Name() = %q, want %q", c.Name(), "binary-divergence")
	}
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil", err)
	}
}

func TestRunningImageFromProc(t *testing.T) {
	procRoot := t.TempDir()
	pidDir := filepath.Join(procRoot, "1234")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	symlink(t, "/opt/gc/bin/gc-main-20260904", filepath.Join(pidDir, "exe"))

	got, err := runningImageFromProc(procRoot, 1234)
	if err != nil {
		t.Fatalf("runningImageFromProc: %v", err)
	}
	if got != "/opt/gc/bin/gc-main-20260904" {
		t.Errorf("runningImageFromProc = %q, want the exe link target", got)
	}
}

func TestRunningImageFromProc_MissingPID(t *testing.T) {
	if _, err := runningImageFromProc(t.TempDir(), 4321); err == nil {
		t.Fatal("runningImageFromProc on a missing pid = nil error, want an error")
	}
}

func TestParseLsofTxtImage(t *testing.T) {
	// Verbatim shape of `lsof -p <pid> -a -d txt -Fn` on macOS.
	out := []byte("p25417\nftxt\nn/Users/op/.gc/bin/gc-main-20260904-bc13ce43a\nftxt\nn/usr/lib/dyld\n")

	got, err := parseLsofTxtImage(out)
	if err != nil {
		t.Fatalf("parseLsofTxtImage: %v", err)
	}
	if got != "/Users/op/.gc/bin/gc-main-20260904-bc13ce43a" {
		t.Errorf("parseLsofTxtImage = %q, want the executable image", got)
	}
}

func TestParseLsofTxtImage_SkipsLibrariesBeforeExecutable(t *testing.T) {
	out := []byte("p1\nftxt\nn/usr/lib/dyld\nftxt\nn/usr/lib/libSystem.B.dylib\nftxt\nn/opt/gc/bin/gc\n")

	got, err := parseLsofTxtImage(out)
	if err != nil {
		t.Fatalf("parseLsofTxtImage: %v", err)
	}
	if got != "/opt/gc/bin/gc" {
		t.Errorf("parseLsofTxtImage = %q, want the executable rather than a mapped library", got)
	}
}

// TestParseLsofTxtImage_KeepsExecutableWithSoInName guards the library filter
// against over-matching: a build artifact whose name merely contains ".so" is
// still the executable.
func TestParseLsofTxtImage_KeepsExecutableWithSoInName(t *testing.T) {
	out := []byte("p1\nftxt\nn/opt/gc/bin/gc.sonoma\nftxt\nn/usr/lib/dyld\n")

	got, err := parseLsofTxtImage(out)
	if err != nil {
		t.Fatalf("parseLsofTxtImage: %v", err)
	}
	if got != "/opt/gc/bin/gc.sonoma" {
		t.Errorf("parseLsofTxtImage = %q, want the executable to survive the library filter", got)
	}
}

func TestParseLsofTxtImage_SkipsVersionedSharedObjects(t *testing.T) {
	out := []byte("p1\nftxt\nn/lib/x86_64-linux-gnu/libc.so.6\nftxt\nn/opt/gc/bin/gc\n")

	got, err := parseLsofTxtImage(out)
	if err != nil {
		t.Fatalf("parseLsofTxtImage: %v", err)
	}
	if got != "/opt/gc/bin/gc" {
		t.Errorf("parseLsofTxtImage = %q, want the versioned .so skipped", got)
	}
}

func TestParseLsofTxtImage_NoEntries(t *testing.T) {
	if _, err := parseLsofTxtImage([]byte("p1\n")); err == nil {
		t.Fatal("parseLsofTxtImage with no txt entries = nil error, want an error")
	}
}

func TestRunningImageResolverFor(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		if runningImageResolverFor(goos) == nil {
			t.Errorf("runningImageResolverFor(%q) = nil, want a resolver", goos)
		}
	}
	if runningImageResolverFor("plan9") != nil {
		t.Error("runningImageResolverFor(\"plan9\") returned a resolver, want nil")
	}
}

package doctor

import (
	"debug/macho"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
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

// imageAt builds a runningImage for a path as it exists right now: the path
// plus the identity of the inode currently behind it. Capturing the identity
// separately from the path is what lets a test later replace the file and
// still describe the process that is executing the original.
func imageAt(t *testing.T, path string) runningImage {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	id, ok := fileIdentityOf(info)
	if !ok {
		t.Skipf("no file identity available on %s", goruntime.GOOS)
	}
	return runningImage{path: path, id: id}
}

// divergenceCheck builds a check with both discovery routes stubbed, so the
// test controls exactly which executed image is compared against which
// PATH binary.
func divergenceCheck(pid int, running runningImage, verified string) *BinaryDivergenceCheck {
	return &BinaryDivergenceCheck{
		supervisorPID:       pid,
		goos:                "testos",
		resolveRunningImage: func(int) (runningImage, error) { return running, nil },
		lookPath:            func(string) (string, error) { return verified, nil },
		versionOf:           func(p string) string { return "version-of-" + filepath.Base(p) },
	}
}

func TestBinaryDivergenceCheck_SameBinary(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeBinary(t, filepath.Join(dir, "gc"), "the one artifact")

	r := divergenceCheck(4242, imageAt(t, bin), bin).Run(&CheckContext{})

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

// TestBinaryDivergenceCheck_ExecutedImageReplacedInPlace is the guard for the
// case the check exists to catch and the one a path-string comparison gets
// wrong. An installer replaces the binary at its path; the process keeps
// executing the original inode. Both sides then report the SAME path — Linux
// marks the image "(deleted)" but macOS does not, so on macOS the path strings
// are indistinguishable and only the identity differs. Comparing paths here
// yields "✓ same binary" for a live divergence.
func TestBinaryDivergenceCheck_ExecutedImageReplacedInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gc")
	writeFakeBinary(t, path, "the bytes the fleet is running")
	executing := imageAt(t, path)

	// rename-over-the-path is what make install, install(1) and a release
	// installer all do; the running process is left on the old inode.
	replacement := writeFakeBinary(t, filepath.Join(dir, "gc.new"), "a different build entirely")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replacing %s: %v", path, err)
	}
	if replaced := imageAt(t, path); replaced.id == executing.id {
		t.Fatal("precondition failed: rename did not change the file identity at the path")
	}

	r := divergenceCheck(28611, executing, path).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning for a replaced-in-place image; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no longer the file at") {
		t.Errorf("Message = %q, want it to say the executed image is no longer the file at its path", r.Message)
	}
	if !strings.Contains(r.Message, path) {
		t.Errorf("Message = %q, want it to name the path %s", r.Message, path)
	}
	if !strings.Contains(r.Message, "28611") {
		t.Errorf("Message = %q, want it to name the supervisor pid", r.Message)
	}
	if r.FixHint == "" {
		t.Error("FixHint is empty; a replaced-in-place image must tell the operator to restart the supervisor")
	}
}

// TestBinaryDivergenceCheck_ExecutedImageRemoved covers the image being
// deleted outright, with nothing put back at its path.
func TestBinaryDivergenceCheck_ExecutedImageRemoved(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gc-old")
	writeFakeBinary(t, gone, "the bytes the fleet is running")
	executing := imageAt(t, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("removing %s: %v", gone, err)
	}
	verified := writeFakeBinary(t, filepath.Join(dir, "gc"), "the build on PATH")

	r := divergenceCheck(31337, executing, verified).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no longer the file at") {
		t.Errorf("Message = %q, want it to report the executed image as gone from its path", r.Message)
	}
}

// TestBinaryDivergenceCheck_UnlinkedFlagBeatsMatchingInode is the inode-reuse
// guard, and the one case where identity alone is not enough. Linux marks a
// replaced image "(deleted)"; if the kernel then hands a new file at that path
// the inode number the old one had, the identities compare equal and the check
// would report agreement for an image that is gone. The platform's own
// statement that the image is unlinked has to win over the number.
func TestBinaryDivergenceCheck_UnlinkedFlagBeatsMatchingInode(t *testing.T) {
	dir := t.TempDir()
	path := writeFakeBinary(t, filepath.Join(dir, "gc"), "bytes")
	executing := imageAt(t, path)
	// The file at the path has exactly the identity of the running image —
	// what inode reuse looks like — but the platform reported it unlinked.
	executing.unlinked = true

	r := divergenceCheck(31337, executing, path).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning: a matching inode must not override an unlinked image; message=%q",
			r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no longer the file at") {
		t.Errorf("Message = %q, want it to report the executed image as gone from its path", r.Message)
	}
}

// TestBinaryDivergenceCheck_UnstattableExecutedImageIsUnverified separates
// "I could not look" from "it was replaced". A permission error is not
// evidence of a replacement and must not be reported as one.
func TestBinaryDivergenceCheck_UnstattableExecutedImageIsUnverified(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	hidden := writeFakeBinary(t, filepath.Join(locked, "gc"), "running")
	executing := imageAt(t, hidden)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	verified := writeFakeBinary(t, filepath.Join(dir, "gc"), "the build on PATH")

	r := divergenceCheck(5, executing, verified).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "unverified") {
		t.Errorf("Message = %q, want it reported as unverified rather than as a replacement", r.Message)
	}
	if strings.Contains(r.Message, "replaced or removed") {
		t.Errorf("Message = %q, must not assert a replacement it never established", r.Message)
	}
}

func TestClassifyImagePath(t *testing.T) {
	dir := t.TempDir()
	path := writeFakeBinary(t, filepath.Join(dir, "gc"), "bytes")
	intact := imageAt(t, path)

	if got := classifyImagePath(intact); got != imagePathIntact {
		t.Errorf("classifyImagePath(intact) = %v, want imagePathIntact", got)
	}

	unlinked := intact
	unlinked.unlinked = true
	if got := classifyImagePath(unlinked); got != imagePathReplaced {
		t.Errorf("classifyImagePath(unlinked) = %v, want imagePathReplaced", got)
	}

	replacement := writeFakeBinary(t, filepath.Join(dir, "gc.new"), "different bytes")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := classifyImagePath(intact); got != imagePathReplaced {
		t.Errorf("classifyImagePath(replaced in place) = %v, want imagePathReplaced", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := classifyImagePath(intact); got != imagePathReplaced {
		t.Errorf("classifyImagePath(removed) = %v, want imagePathReplaced", got)
	}
}

// TestBinaryDivergenceCheck_SymlinksToOneArtifact covers the healthy end state
// of vc-lwif: several PATH entries all pointing at one dated artifact. A false
// positive here would fire on every correctly converged deployment.
func TestBinaryDivergenceCheck_SymlinksToOneArtifact(t *testing.T) {
	dir := t.TempDir()
	artifact := writeFakeBinary(t, filepath.Join(dir, "gc-main-20260904-bc13ce43a"), "the one artifact")
	viaAnotherPath := symlink(t, artifact, filepath.Join(dir, "gc-b"))

	// The executed image is always reported by its real path; PATH reaches the
	// same inode through a symlink.
	r := divergenceCheck(4242, imageAt(t, artifact), viaAnotherPath).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for a symlink to the executed artifact; message=%q details=%v",
			r.Status, r.Message, r.Details)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), viaAnotherPath) {
		t.Errorf("Details do not name the PATH entry %s: %v", viaAnotherPath, r.Details)
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

	r := divergenceCheck(4242, imageAt(t, artifact), linked).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for two links to one inode; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "one file") {
		t.Errorf("Message = %q, want mention that the two paths are one file", r.Message)
	}
}

// TestBinaryDivergenceCheck_IdenticalContentDistinctFiles covers two real
// copies of one build. Different inodes, same bytes: the fleet runs what the
// probe verified, so this must not warn.
func TestBinaryDivergenceCheck_IdenticalContentDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	a := writeFakeBinary(t, filepath.Join(dir, "gc-a"), "identical bytes")
	b := writeFakeBinary(t, filepath.Join(dir, "gc-b"), "identical bytes")

	r := divergenceCheck(4242, imageAt(t, a), b).Run(&CheckContext{})

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

	r := divergenceCheck(28611, imageAt(t, executed), verified).Run(&CheckContext{})

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

	r := divergenceCheck(1, imageAt(t, executed), verified).Run(&CheckContext{})

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
		resolveRunningImage: func(int) (runningImage, error) {
			t.Error("resolveRunningImage called with no supervisor running")
			return runningImage{}, nil
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
	c := divergenceCheck(77, runningImage{}, "")
	c.resolveRunningImage = func(int) (runningImage, error) {
		return runningImage{}, fmt.Errorf("operation not permitted")
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "unverified") {
		t.Errorf("Message = %q, want it reported as unverified, not as a divergence", r.Message)
	}
	if !strings.Contains(r.Message, "operation not permitted") {
		t.Errorf("Message = %q, want the underlying error", r.Message)
	}
}

// TestBinaryDivergenceCheck_UnstattablePathBinaryIsUnverified guards the
// difference between "I could not look" and "they differ". A supervisor
// running as another user, or a binary behind a 0700 directory, must not be
// reported as a positive divergence finding.
func TestBinaryDivergenceCheck_UnstattablePathBinaryIsUnverified(t *testing.T) {
	dir := t.TempDir()
	executed := writeFakeBinary(t, filepath.Join(dir, "gc"), "running")
	missing := filepath.Join(dir, "does-not-exist")

	r := divergenceCheck(5, imageAt(t, executed), missing).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "unverified") {
		t.Errorf("Message = %q, want it reported as unverified", r.Message)
	}
	if strings.Contains(r.Message, "is executing") {
		t.Errorf("Message = %q, must not assert a divergence it never established", r.Message)
	}
}

func TestBinaryDivergenceCheck_NotOnPath(t *testing.T) {
	dir := t.TempDir()
	executed := writeFakeBinary(t, filepath.Join(dir, "gc"), "running")
	c := divergenceCheck(5, imageAt(t, executed), "")
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
	target := writeFakeBinary(t, filepath.Join(procRoot, "gc-main-20260904"), "image")
	symlink(t, target, filepath.Join(pidDir, "exe"))

	got, err := runningImageFromProc(procRoot, 1234)
	if err != nil {
		t.Fatalf("runningImageFromProc: %v", err)
	}
	if got.path != target {
		t.Errorf("path = %q, want the exe link target %q", got.path, target)
	}
	if got.unlinked {
		t.Error("unlinked = true for a live image, want false")
	}
	// The identity must come from the executing inode, not be left zero:
	// every comparison downstream depends on it.
	if want := imageAt(t, target); got.id != want.id {
		t.Errorf("id = %+v, want the identity of the executing inode %+v", got.id, want.id)
	}
}

func TestRunningImageFromProc_MissingPID(t *testing.T) {
	if _, err := runningImageFromProc(t.TempDir(), 4321); err == nil {
		t.Fatal("runningImageFromProc on a missing pid = nil error, want an error")
	}
}

func TestParseLsofFileEntries(t *testing.T) {
	// Verbatim shape of `lsof -p <pid> -a -d txt -b -w -FnDi` on macOS.
	out := []byte("p25417\n" +
		"ftxt\nD0x100000f\ni1383293036\nn/Users/op/.gc/bin/gc-main-20260904\n" +
		"ftxt\nD0x100000f\ni1152921500312573255\nn/usr/lib/dyld\n")

	entries := parseLsofFileEntries(out)

	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	if entries[0].name != "/Users/op/.gc/bin/gc-main-20260904" {
		t.Errorf("entries[0].name = %q", entries[0].name)
	}
	if entries[0].dev != "0x100000f" || entries[0].ino != "1383293036" {
		t.Errorf("entries[0] identity fields = dev %q ino %q, want the lsof-reported pair", entries[0].dev, entries[0].ino)
	}
	if entries[1].name != "/usr/lib/dyld" {
		t.Errorf("entries[1].name = %q", entries[1].name)
	}
}

// TestSelectRunningImage_SkipsMappedDataFiles is the guard for the other way
// this resolver can name the wrong file. A gc binary links ICU, so its
// mapped-text set contains locale tables and icudt*.dat alongside the
// executable — none of which any suffix denylist would catch. Selection asks
// whether the candidate is itself an executable.
func TestSelectRunningImage_SkipsMappedDataFiles(t *testing.T) {
	entries := []lsofEntry{
		{name: "/usr/lib/dyld", dev: "0x1", ino: "10"},
		{name: "/opt/homebrew/opt/icu4c/share/icu/78.3/icudt78l.dat", dev: "0x1", ino: "11"},
		{name: "/usr/share/locale/en_US.UTF-8/LC_COLLATE", dev: "0x1", ino: "12"},
		{name: "/System/Library/.../Assets.car", dev: "0x1", ino: "13"},
		{name: "/opt/homebrew/Cellar/icu4c@78/78.3/lib/libicuuc.78.3.dylib", dev: "0x1", ino: "14"},
		{name: "/opt/gc/bin/gc-main-20260904", dev: "0x1", ino: "15"},
	}
	executables := map[string]bool{"/opt/gc/bin/gc-main-20260904": true}

	got, err := selectRunningImage(entries, func(p string) bool { return executables[p] })
	if err != nil {
		t.Fatalf("selectRunningImage: %v", err)
	}
	if got.path != "/opt/gc/bin/gc-main-20260904" {
		t.Errorf("path = %q, want the executable rather than a mapped data file or library", got.path)
	}
	if got.id.ino != 15 {
		t.Errorf("id.ino = %d, want 15 (the executable's inode)", got.id.ino)
	}
}

func TestSelectRunningImage_MarksUnlinkedImage(t *testing.T) {
	entries := []lsofEntry{{name: "/opt/gc/bin/gc" + deletedImageSuffix, dev: "0x1", ino: "9"}}

	got, err := selectRunningImage(entries, func(string) bool { return true })
	if err != nil {
		t.Fatalf("selectRunningImage: %v", err)
	}
	if got.path != "/opt/gc/bin/gc" {
		t.Errorf("path = %q, want the suffix stripped", got.path)
	}
	if !got.unlinked {
		t.Error("unlinked = false, want true for an image lsof marked deleted")
	}
}

// TestSelectRunningImage_MissingIdentityIsAnError asserts the resolver fails
// loudly rather than returning a zero identity that would compare equal to
// another zero identity.
func TestSelectRunningImage_MissingIdentityIsAnError(t *testing.T) {
	entries := []lsofEntry{{name: "/opt/gc/bin/gc", ino: "9"}} // no device field

	if _, err := selectRunningImage(entries, func(string) bool { return true }); err == nil {
		t.Fatal("selectRunningImage with no device number = nil error, want an error")
	}
}

func TestSelectRunningImage_NoExecutable(t *testing.T) {
	entries := []lsofEntry{{name: "/usr/lib/dyld", dev: "0x1", ino: "1"}}

	if _, err := selectRunningImage(entries, func(string) bool { return false }); err == nil {
		t.Fatal("selectRunningImage with no executable entry = nil error, want an error")
	}
}

func TestParseLsofIdentity(t *testing.T) {
	// 0x100000f is the device this machine's lsof reports for /var; it must
	// decode to the same number stat(2) gives (16777231).
	got, err := parseLsofIdentity(lsofEntry{dev: "0x100000f", ino: "1383293036"})
	if err != nil {
		t.Fatalf("parseLsofIdentity: %v", err)
	}
	if got.dev != 16777231 {
		t.Errorf("dev = %d, want 16777231 (0x100000f)", got.dev)
	}
	if got.ino != 1383293036 {
		t.Errorf("ino = %d, want 1383293036", got.ino)
	}
}

func TestParseLsofIdentity_Rejects(t *testing.T) {
	// wantMsg pins the wording for the absent-field cases: "lsof told us
	// nothing" is a different operator-facing fact from "lsof told us
	// something unparseable", and the check reports which.
	for name, tc := range map[string]struct {
		entry   lsofEntry
		wantMsg string
	}{
		"no device":      {lsofEntry{ino: "1"}, "reported no device number"},
		"no inode":       {lsofEntry{dev: "0x1"}, "reported no inode number"},
		"blank device":   {lsofEntry{dev: "   ", ino: "1"}, "reported no device number"},
		"bad device":     {lsofEntry{dev: "0xzz", ino: "1"}, "parsing device number"},
		"bad inode":      {lsofEntry{dev: "0x1", ino: "twelve"}, "parsing inode number"},
		"nothing at all": {lsofEntry{}, "reported no device number"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseLsofIdentity(tc.entry)
			if err == nil {
				t.Fatalf("parseLsofIdentity(%+v) = nil error, want an error", tc.entry)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantMsg)
			}
		})
	}
}

// machOHeader builds a minimal, valid 64-bit Mach-O header of the given type
// (no load commands), so the predicate's discriminator can be tested on every
// filetype rather than only on whatever the host happens to ship.
func machOHeader(filetype uint32) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b[0:], macho.Magic64)
	binary.LittleEndian.PutUint32(b[4:], uint32(macho.CpuArm64))
	binary.LittleEndian.PutUint32(b[8:], 0) // cpusubtype
	binary.LittleEndian.PutUint32(b[12:], filetype)
	binary.LittleEndian.PutUint32(b[16:], 0) // ncmds
	binary.LittleEndian.PutUint32(b[20:], 0) // sizeofcmds
	binary.LittleEndian.PutUint32(b[24:], 0) // flags
	binary.LittleEndian.PutUint32(b[28:], 0) // reserved
	return b
}

// fatMachO wraps one thin Mach-O slice in a universal-binary header.
func fatMachO(thin []byte) []byte {
	const offset = 4096
	b := make([]byte, offset+len(thin))
	binary.BigEndian.PutUint32(b[0:], macho.MagicFat)
	binary.BigEndian.PutUint32(b[4:], 1) // nfat_arch
	binary.BigEndian.PutUint32(b[8:], uint32(macho.CpuArm64))
	binary.BigEndian.PutUint32(b[12:], 0) // cpusubtype
	binary.BigEndian.PutUint32(b[16:], offset)
	binary.BigEndian.PutUint32(b[20:], uint32(len(thin)))
	binary.BigEndian.PutUint32(b[24:], 12) // align
	copy(b[offset:], thin)
	return b
}

func writeBytes(t *testing.T, path string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// TestMachOIsExecutable pins the discriminator itself: only MH_EXECUTE
// qualifies. A dylib and the dynamic loader are Mach-O too and parse cleanly,
// so a predicate that merely asks "is this Mach-O?" would name a mapped
// library as the executed image.
func TestMachOIsExecutable(t *testing.T) {
	dir := t.TempDir()

	for name, tc := range map[string]struct {
		content []byte
		want    bool
	}{
		"executable":      {machOHeader(uint32(macho.TypeExec)), true},
		"dylib":           {machOHeader(uint32(macho.TypeDylib)), false},
		"bundle":          {machOHeader(uint32(macho.TypeBundle)), false},
		"object file":     {machOHeader(uint32(macho.TypeObj)), false},
		"fat executable":  {fatMachO(machOHeader(uint32(macho.TypeExec))), true},
		"fat dylib":       {fatMachO(machOHeader(uint32(macho.TypeDylib))), false},
		"mapped data":     {[]byte("icu locale table, not a mach-o file at all"), false},
		"truncated magic": {[]byte{0xcf, 0xfa}, false},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeBytes(t, filepath.Join(dir, strings.ReplaceAll(name, " ", "-")), tc.content)
			if got := machOIsExecutable(path); got != tc.want {
				t.Errorf("machOIsExecutable(%s) = %v, want %v", name, got, tc.want)
			}
		})
	}

	// And the real thing: this test binary is a Mach-O executable on darwin.
	if goruntime.GOOS == "darwin" {
		self, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable: %v", err)
		}
		if !machOIsExecutable(self) {
			t.Errorf("machOIsExecutable(%s) = false for this test binary, want true", self)
		}
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

// TestFileIdentityOfDistinguishesFiles is the floor the whole check stands on:
// two different files must not share an identity, and one file reached by two
// names must.
func TestFileIdentityOfDistinguishesFiles(t *testing.T) {
	dir := t.TempDir()
	a := writeFakeBinary(t, filepath.Join(dir, "a"), "same bytes")
	b := writeFakeBinary(t, filepath.Join(dir, "b"), "same bytes")

	idA, okA := fileIdentityOf(mustStat(t, a))
	idB, okB := fileIdentityOf(mustStat(t, b))
	if !okA || !okB {
		t.Skipf("no file identity available on %s", goruntime.GOOS)
	}
	if idA == idB {
		t.Error("two distinct files share a file identity; the check cannot tell them apart")
	}

	link := filepath.Join(dir, "a-link")
	if err := os.Link(a, link); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}
	idLink, _ := fileIdentityOf(mustStat(t, link))
	if idLink != idA {
		t.Errorf("one file reached by two names has different identities %+v vs %+v", idLink, idA)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info
}

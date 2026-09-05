package doctor

import (
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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

// linkTo hard-links newname to oldname and returns newname: a second route to
// one inode, which is what a platform handle on an executing image behaves
// like. Skips where the filesystem has no hard links.
func linkTo(t *testing.T, oldname, newname string) string {
	t.Helper()
	if err := os.Link(oldname, newname); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}
	return newname
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

	// Not StatusOK: a green check for a comparison that never ran is the
	// failure mode this check exists to surface one level up.
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "unverified") {
		t.Errorf("Message = %q, want the same unverified wording every other did-not-compare outcome uses", r.Message)
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
	// The error exec.LookPath actually returns. A lookalike string would pass
	// while asserting nothing about which lookup failures mean "gc is absent".
	c.lookPath = func(name string) (string, error) { return "", &exec.Error{Name: name, Err: exec.ErrNotFound} }

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
	// The magic link is also a readable handle on the running bytes. Without
	// it, a replaced image on Linux degrades from "here is the answer" to
	// "the bytes are unreachable" — the whole reason procfs beats lsof here.
	if got.contentPath != filepath.Join(procRoot, "1234", "exe") {
		t.Errorf("contentPath = %q, want the /proc exe link so replaced images stay comparable", got.contentPath)
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

// TestSelectRunningImage_DeletedImageIsReportedNotSkipped is the guard for an
// image that is gone from disk — the state a fleet enters the moment someone
// prunes a dated artifact while a supervisor is executing it. The executable
// test reads the file at the reported path, and that read fails when the file
// has been deleted; skipping the entry on that basis would turn the sharpest
// finding available (the fleet is running bytes that exist nowhere on disk)
// into "unverified", pointing the operator at a permissions problem instead.
func TestSelectRunningImage_DeletedImageIsReportedNotSkipped(t *testing.T) {
	// The real predicate against paths that do not exist: nothing here can be
	// read, let alone parsed as Mach-O.
	dir := t.TempDir()
	loader := writeFakeBinary(t, filepath.Join(dir, "dyld"), "the dynamic loader")
	loaderID := identityOf(t, loader)
	entries := []lsofEntry{
		{name: filepath.Join(dir, "gc-main-20260904-deadbeef"), dev: "0x100000f", ino: "1386167869"},
		{name: loader, dev: fmt.Sprintf("%#x", loaderID.dev), ino: fmt.Sprint(loaderID.ino)},
	}

	got, err := selectRunningImage(entries, machOIsExecutable)
	if err != nil {
		t.Fatalf("selectRunningImage on a deleted image = %v, want the entry reported", err)
	}
	if got.path != entries[0].name {
		t.Errorf("path = %q, want the executed image %q", got.path, entries[0].name)
	}
	if got.id.ino != 1386167869 {
		t.Errorf("id.ino = %d, want 1386167869 — the identity is what survives the file", got.id.ino)
	}
}

// TestSelectRunningImage_SoleUnevaluatableEntryIsTheImage covers the fallback
// with the predicate stubbed, in the shell-wrapper shape: the executable's
// path replaced by something readable that is not a Mach-O image, every
// library beside it still exactly the file lsof recorded.
//
// The libraries are real files with their real identities, because that is the
// state the fallback's reasoning depends on: pass 1 established that none of
// the entries it could evaluate is an executable, so the one entry it could
// not evaluate is the image.
func TestSelectRunningImage_SoleUnevaluatableEntryIsTheImage(t *testing.T) {
	dir := t.TempDir()
	image := writeFakeBinary(t, filepath.Join(dir, "gc"), "the running image")
	loader := writeFakeBinary(t, filepath.Join(dir, "dyld"), "the dynamic loader")
	imageID, loaderID := identityOf(t, image), identityOf(t, loader)
	entries := []lsofEntry{
		{name: image, dev: fmt.Sprintf("%#x", imageID.dev), ino: fmt.Sprint(imageID.ino)},
		{name: loader, dev: fmt.Sprintf("%#x", loaderID.dev), ino: fmt.Sprint(loaderID.ino)},
	}
	// The wrapper install: a new inode at the executable's path, holding
	// something that is not a Mach-O image.
	wrapper := writeFakeBinary(t, filepath.Join(dir, "gc.wrapper"), "#!/bin/sh\nexec gc-real \"$@\"\n")
	if err := os.Rename(wrapper, image); err != nil {
		t.Fatalf("replacing %s: %v", image, err)
	}

	got, err := selectRunningImage(entries, func(string) bool { return false })
	if err != nil {
		t.Fatalf("selectRunningImage = %v, want the replaced entry reported", err)
	}
	if got.path != image {
		t.Errorf("path = %q, want the replaced executable %q rather than the untouched loader", got.path, image)
	}
	if got.id != imageID {
		t.Errorf("id = %+v, want the identity lsof recorded %+v", got.id, imageID)
	}
}

// TestBinaryDivergenceCheck_ExecutedImageDeletedEndToEnd walks the deleted
// image all the way through Run, because the selection fallback is only
// worth having if the check then reports it as a divergence.
func TestBinaryDivergenceCheck_ExecutedImageDeletedEndToEnd(t *testing.T) {
	dir := t.TempDir()
	artifact := writeFakeBinary(t, filepath.Join(dir, "gc-main-20260904-deadbeef"), "the bytes the fleet runs")
	executing := imageAt(t, artifact)
	loader := writeFakeBinary(t, filepath.Join(dir, "dyld"), "the dynamic loader")
	loaderID := identityOf(t, loader)
	entries := []lsofEntry{
		{name: artifact, dev: fmt.Sprintf("%#x", executing.id.dev), ino: fmt.Sprint(executing.id.ino)},
		{name: loader, dev: fmt.Sprintf("%#x", loaderID.dev), ino: fmt.Sprint(loaderID.ino)},
	}
	if err := os.Remove(artifact); err != nil {
		t.Fatalf("pruning %s: %v", artifact, err)
	}
	verified := writeFakeBinary(t, filepath.Join(dir, "gc"), "the build on PATH")

	c := divergenceCheck(4242, runningImage{}, verified)
	c.resolveRunningImage = func(int) (runningImage, error) {
		return selectRunningImage(entries, machOIsExecutable)
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if strings.Contains(r.Message, "unverified") {
		t.Errorf("Message = %q, must report the divergence rather than withhold it", r.Message)
	}
	if !strings.Contains(r.Message, "no longer the file at") {
		t.Errorf("Message = %q, want it to say the image is gone from its path", r.Message)
	}
	if !strings.Contains(r.FixHint, "restart the supervisor") {
		t.Errorf("FixHint = %q, want the restart advice, not a permissions hint", r.FixHint)
	}
}

func TestSelectRunningImage_NoEntries(t *testing.T) {
	_, err := selectRunningImage(nil, func(string) bool { return true })
	if err == nil {
		t.Fatal("selectRunningImage with no entries = nil error, want an error")
	}
	// "lsof told us nothing" is a different diagnosis from "nothing lsof told
	// us was the image", and the operator-facing message says which.
	if !strings.Contains(err.Error(), "no mapped-text entries") {
		t.Errorf("error = %q, want it to say lsof reported nothing at all", err)
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

// TestStatFieldToUint64 pins the contract that an unrecognized field type is
// reported as unavailable rather than as zero. A zero device on the disk side
// against a real one from lsof would read as a divergence on a healthy host.
func TestStatFieldToUint64(t *testing.T) {
	for name, tc := range map[string]struct {
		in     any
		want   uint64
		wantOK bool
	}{
		"uint64":            {uint64(42), 42, true},
		"int64":             {int64(42), 42, true},
		"uint32":            {uint32(42), 42, true},
		"int32":             {int32(42), 42, true},
		"negative int32":    {int32(-1), 0xffffffff, true},
		"unknown type":      {"not a number", 0, false},
		"unknown int width": {int16(42), 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := statFieldToUint64(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("value = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCompareContent covers the three gates, including the tail-only
// difference the bounded probe exists to settle without reading either file
// whole, and the unreadable case that must never read as "differs".
func TestCompareContent(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("gc", contentChunkSize)

	facts := func(name, content string) binaryFacts {
		return statBinary(writeFakeBinary(t, filepath.Join(dir, name), content))
	}

	identicalA := facts("identical-a", body)
	identicalB := facts("identical-b", body)
	outcome, reason, err := compareContent(identicalA, identicalB)
	if outcome != contentSame || err != nil {
		t.Errorf("compareContent(identical) = %v, %v; want contentSame", outcome, err)
	}
	if !strings.Contains(reason, "sha256") {
		t.Errorf("reason = %q, want it to carry a digest", reason)
	}

	for name, other := range map[string]binaryFacts{
		"different size":    facts("shorter", body[:len(body)-2]),
		"differs at head":   facts("head-diff", "X"+body[1:]),
		"differs at tail":   facts("tail-diff", body[:len(body)-1]+"X"),
		"differs in middle": facts("middle-diff", body[:len(body)/2]+"X"+body[len(body)/2+1:]),
	} {
		t.Run(name, func(t *testing.T) {
			if outcome, _, _ := compareContent(identicalA, other); outcome != contentDiffers {
				t.Errorf("compareContent = %v, want contentDiffers", outcome)
			}
		})
	}

	t.Run("unstattable is unknown", func(t *testing.T) {
		if outcome, _, err := compareContent(identicalA, binaryFacts{}); outcome != contentUnknown || err == nil {
			t.Errorf("compareContent with an unstattable side = %v, %v; want contentUnknown and an error", outcome, err)
		}
	})
}

// TestCompareContent_UnreadableIsUnknown is the guard for the third time on
// this check that "could not look" was rendered as "they differ". Two
// byte-identical copies where the executed one is stattable but not readable
// must report unknown: a permission error is not evidence of a difference.
func TestCompareContent_UnreadableIsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	body := strings.Repeat("gc", contentChunkSize)
	readable := statBinary(writeFakeBinary(t, filepath.Join(dir, "readable"), body))
	locked := writeFakeBinary(t, filepath.Join(dir, "locked"), body)
	if err := os.Chmod(locked, 0o111); err != nil { // executable, not readable
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	lockedFacts := statBinary(locked)

	outcome, _, err := compareContent(lockedFacts, readable)

	if outcome != contentUnknown {
		t.Fatalf("compareContent on an unreadable binary = %v, want contentUnknown", outcome)
	}
	if err == nil {
		t.Error("error is nil; the caller needs it to explain why nothing was established")
	}
}

// TestCompareContent_UnreadableStreamIsUnknown reaches the streaming pass's
// error path, which the test above cannot: sameTail opens both files first and
// fails there. Empty files skip the tail probe entirely (nothing to sample), so
// an unreadable empty file is refused for the first time inside
// compareAndDigest.
func TestCompareContent_UnreadableStreamIsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	readable := statBinary(writeFakeBinary(t, filepath.Join(dir, "empty-readable"), ""))
	locked := writeFakeBinary(t, filepath.Join(dir, "empty-locked"), "")
	if err := os.Chmod(locked, 0o111); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	lockedFacts := statBinary(locked)

	// Precondition: the tail probe must not be what refuses this pair, or the
	// test would prove nothing about the streaming pass.
	if same, err := sameTail(lockedFacts.readPath(), readable.readPath(), 0); err != nil || !same {
		t.Fatalf("precondition: sameTail on empty files = %v, %v; want true, nil", same, err)
	}

	outcome, _, err := compareContent(lockedFacts, readable)

	if outcome != contentUnknown {
		t.Fatalf("compareContent = %v, want contentUnknown when the stream cannot be read", outcome)
	}
	if err == nil {
		t.Error("error is nil; the caller needs it to explain why nothing was established")
	}
}

// TestBinaryDivergenceCheck_UnreadableExecutedImageIsUnverified walks the same
// case through Run: identical copies, executed side unreadable. Reporting a
// divergence here would tell an operator to converge two artifacts that are
// already the same bytes.
func TestBinaryDivergenceCheck_UnreadableExecutedImageIsUnverified(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	body := strings.Repeat("gc", 64)
	executed := writeFakeBinary(t, filepath.Join(dir, "executed"), body)
	verified := writeFakeBinary(t, filepath.Join(dir, "gc"), body)
	img := imageAt(t, executed)
	if err := os.Chmod(executed, 0o111); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(executed, 0o755) })

	r := divergenceCheck(4242, img, verified).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "unverified") {
		t.Errorf("Message = %q, want it reported as unverified", r.Message)
	}
	if strings.Contains(r.Message, "binary divergence: ") {
		t.Errorf("Message = %q, must not assert a divergence it never established", r.Message)
	}
}

// TestSameContentTailProbeSettlesTailDifference pins where the answer comes
// from, not just what it is. sameTail is what bounds the cost of the one shape
// the streaming pass would otherwise read two whole binaries to settle — same
// size, identical until the very end. Called directly, because through
// compareContent the streaming pass would return the same answer and hide whether
// the probe did anything at all.
func TestSameContentTailProbeSettlesTailDifference(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("gc", contentChunkSize) // twice the sample size
	a := writeFakeBinary(t, filepath.Join(dir, "a"), body)
	b := writeFakeBinary(t, filepath.Join(dir, "b"), body[:len(body)-1]+"X")

	same, err := sameTail(a, b, int64(len(body)))
	if err != nil {
		t.Fatalf("sameTail: %v", err)
	}
	if same {
		t.Error("sameTail = true for files differing only in their last byte; the probe is not sampling the end")
	}

	if same, err := sameTail(a, a, int64(len(body))); err != nil || !same {
		t.Errorf("sameTail(a, a) = %v, %v; a file must match itself", same, err)
	}
}

// TestBinaryDivergenceCheck_ReplacedImageWithIdenticalBytesIsOK covers an
// idempotent reinstall: `make install` re-run at the same commit while the
// supervisor is up leaves a new inode holding identical bytes. The fleet is
// running exactly what a probe verifies, so warning "replaced under it,
// restart" would be a false alarm. Where the platform keeps a handle on the
// executing inode (Linux's /proc/<pid>/exe), the question is answerable and
// the check answers it instead of guessing.
func TestBinaryDivergenceCheck_ReplacedImageWithIdenticalBytesIsOK(t *testing.T) {
	dir := t.TempDir()
	body := "the build both sides hold"
	path := filepath.Join(dir, "gc-artifact")
	writeFakeBinary(t, path, body)
	executing := imageAt(t, path)
	// A handle on the executing inode itself, as /proc/<pid>/exe is. A hard
	// link is the portable stand-in: a second route to the same bytes that
	// survives the loss of the original name.
	executing.contentPath = linkTo(t, path, filepath.Join(dir, "exe-handle"))

	// The reinstall: a new inode at the same path, same bytes.
	replacement := writeFakeBinary(t, filepath.Join(dir, "gc-artifact.new"), body)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("reinstalling %s: %v", path, err)
	}
	verified := writeFakeBinary(t, filepath.Join(dir, "gc"), body)

	r := divergenceCheck(4242, executing, verified).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for a reinstall of identical bytes; message=%q", r.Status, r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "bytes are unchanged") {
		t.Errorf("Details do not record that the artifact churned without changing: %v", r.Details)
	}
	// The handle path is an implementation detail; an operator should see the
	// path they know.
	if strings.Contains(r.Message, "exe-handle") || strings.Contains(joined, "exe-handle") {
		t.Errorf("output names the platform handle rather than the operator-facing path: %q / %v", r.Message, r.Details)
	}
}

// TestBinaryDivergenceCheck_ReplacedImageWithDifferentBytesDiverges is the
// other half: a handle exists, the bytes really differ, so the check gives the
// definite answer rather than the weaker "unreachable" one.
func TestBinaryDivergenceCheck_ReplacedImageWithDifferentBytesDiverges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gc-artifact")
	writeFakeBinary(t, path, "the bytes the fleet runs")
	executing := imageAt(t, path)
	executing.contentPath = linkTo(t, path, filepath.Join(dir, "exe-handle"))

	replacement := writeFakeBinary(t, filepath.Join(dir, "gc-artifact.new"), "a different build")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replacing %s: %v", path, err)
	}
	verified := writeFakeBinary(t, filepath.Join(dir, "gc"), "a different build")

	r := divergenceCheck(4242, executing, verified).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning", r.Status)
	}
	if !strings.Contains(r.Message, "binary divergence: ") {
		t.Errorf("Message = %q, want the definite divergence verdict the handle makes possible", r.Message)
	}
	if strings.Contains(r.Message, "exe-handle") {
		t.Errorf("Message = %q, want the operator-facing path, not the handle", r.Message)
	}
}

// TestBinaryDivergenceCheck_PermissionErrorHintIsActionable guards the hint,
// not just the status. A supervisor running as another user produces this
// every run; "re-run once it is readable" is advice the operator cannot act on.
func TestBinaryDivergenceCheck_PermissionErrorHintIsActionable(t *testing.T) {
	c := divergenceCheck(4242, runningImage{}, "")
	c.resolveRunningImage = func(int) (runningImage, error) {
		return runningImage{}, fmt.Errorf("reading /proc/4242/exe: %w", fs.ErrPermission)
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning", r.Status)
	}
	if !strings.Contains(r.Message, "unverified") {
		t.Errorf("Message = %q, want it reported as unverified", r.Message)
	}
	if !strings.Contains(r.FixHint, "another user") || !strings.Contains(r.FixHint, "sudo") {
		t.Errorf("FixHint = %q, want it to name the cause and something the operator can do", r.FixHint)
	}
}

// TestSelectRunningImage_NeverNamesALibrary guards the fallback's blind spot.
// Naming a mapped library as the executed image — with a restart hint attached
// — is worse than reporting that it could not be determined.
func TestSelectRunningImage_NeverNamesALibrary(t *testing.T) {
	dir := t.TempDir()
	// Every entry is a real file that is still exactly what lsof recorded, so
	// nothing here is the replaced-or-deleted image.
	lib := writeFakeBinary(t, filepath.Join(dir, "libicuuc.78.3.dylib"), "library")
	data := writeFakeBinary(t, filepath.Join(dir, "icudt78l.dat"), "locale table")
	entries := []lsofEntry{
		{name: lib, dev: fmt.Sprintf("%#x", identityOf(t, lib).dev), ino: fmt.Sprint(identityOf(t, lib).ino)},
		{name: data, dev: fmt.Sprintf("%#x", identityOf(t, data).dev), ino: fmt.Sprint(identityOf(t, data).ino)},
	}

	_, err := selectRunningImage(entries, func(string) bool { return false })

	if err == nil {
		t.Fatal("selectRunningImage named one of the mapped libraries as the executed image; want an error")
	}
}

func identityOf(t *testing.T, path string) fileIdentity {
	t.Helper()
	id, ok := fileIdentityOf(mustStat(t, path))
	if !ok {
		t.Skipf("no file identity available on %s", goruntime.GOOS)
	}
	return id
}

func TestDescribeSkew(t *testing.T) {
	// "NEWER by 0s" is self-contradictory; sub-second skew is still skew.
	if got := describeSkew(400 * time.Millisecond); got != "by under a second" {
		t.Errorf("describeSkew(400ms) = %q, want %q", got, "by under a second")
	}
	if got := describeSkew(90 * time.Second); got != "by 1m30s" {
		t.Errorf("describeSkew(90s) = %q, want %q", got, "by 1m30s")
	}
}

// TestSkewLineNeverReportsZero walks the sub-second case through the line an
// operator actually reads.
func TestSkewLineNeverReportsZero(t *testing.T) {
	dir := t.TempDir()
	older := statBinary(writeFakeBinary(t, filepath.Join(dir, "older"), "a"))
	newer := statBinary(writeFakeBinary(t, filepath.Join(dir, "newer"), "b"))
	base := time.Now()
	if err := os.Chtimes(older.realPath, base, base); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	half := base.Add(500 * time.Millisecond)
	if err := os.Chtimes(newer.realPath, half, half); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	line := skewLine(statBinary(older.realPath), statBinary(newer.realPath))

	if strings.Contains(line, "by 0s") {
		t.Errorf("skewLine = %q, want no self-contradictory zero duration", line)
	}
	if !strings.Contains(line, "NEWER") {
		t.Errorf("skewLine = %q, want it to still report a direction", line)
	}
}

// TestSelectRunningImage_UnreadableImageIsIdentified covers a running image
// that is present and unchanged but that this user cannot read — a release
// installer's root-owned 0111 artifact, say. The executable test cannot
// evaluate it, and treating that as "not the image" leaves the check reporting
// only that it found nothing. Identifying it lets the comparison say what is
// actually wrong: the image is right there, and unreadable.
func TestSelectRunningImage_UnreadableImageIsIdentified(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	image := writeFakeBinary(t, filepath.Join(dir, "gc-main-20260904"), "the running image")
	lib := writeFakeBinary(t, filepath.Join(dir, "libicuuc.78.3.dylib"), "a readable, unchanged library")
	imageID, libID := identityOf(t, image), identityOf(t, lib)
	if err := os.Chmod(image, 0o111); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(image, 0o755) })

	entries := []lsofEntry{
		{name: lib, dev: fmt.Sprintf("%#x", libID.dev), ino: fmt.Sprint(libID.ino)},
		{name: image, dev: fmt.Sprintf("%#x", imageID.dev), ino: fmt.Sprint(imageID.ino)},
	}

	got, err := selectRunningImage(entries, machOIsExecutable)
	if err != nil {
		t.Fatalf("selectRunningImage on an unreadable image = %v, want it identified", err)
	}
	if got.path != image {
		t.Errorf("path = %q, want the unreadable image %q rather than the readable library", got.path, image)
	}
}

func TestFileIsReadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	readable := writeFakeBinary(t, filepath.Join(dir, "readable"), "x")
	if !fileIsReadable(readable) {
		t.Error("fileIsReadable = false for a readable file")
	}
	locked := writeFakeBinary(t, filepath.Join(dir, "locked"), "x")
	if err := os.Chmod(locked, 0o111); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if fileIsReadable(locked) {
		t.Error("fileIsReadable = true for an execute-only file")
	}
	if fileIsReadable(filepath.Join(dir, "absent")) {
		t.Error("fileIsReadable = true for a path that does not exist")
	}
}

// TestBinaryDivergenceCheck_ContentHandleNamingAnotherFileIsNotTrusted is the
// guard for the one route in this check that can still produce a green ✓ for a
// fleet running different bytes.
//
// /proc/<pid>/exe is a magic link: opening or stat'ing it lands on the
// executing inode, but *resolving* it as a symlink lands on whatever now sits
// at the path it names. Reading the executed image through that resolution
// therefore reports the replacement — and when the replacement is the binary
// on PATH, as it is after any in-place install, the comparison answers "the
// same binary" about a file the process is not executing.
//
// The world state below is one no other fixture builds: a contentPath handle
// that resolves to different bytes than the running identity. The two existing
// contentPath tests both supply a handle holding the original bytes, which is
// why nothing caught this.
func TestBinaryDivergenceCheck_ContentHandleNamingAnotherFileIsNotTrusted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gc-artifact")
	writeFakeBinary(t, path, "the bytes the fleet is running")
	executing := imageAt(t, path)
	// The handle names the path, as /proc/<pid>/exe does. A plain symlink is
	// what resolving the magic link degrades to.
	executing.contentPath = symlink(t, path, filepath.Join(dir, "exe-link"))

	// The in-place install: the replacement and the PATH binary hold the same
	// bytes as each other, and different bytes from the running image.
	installed := "the build that was installed over it"
	replacement := writeFakeBinary(t, filepath.Join(dir, "gc-artifact.new"), installed)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replacing %s: %v", path, err)
	}
	verified := writeFakeBinary(t, filepath.Join(dir, "gc"), installed)

	r := divergenceCheck(4242, executing, verified).Run(&CheckContext{})

	if r.Status == StatusOK {
		t.Fatalf("Status = StatusOK: the check read bytes through a stale name and called them the running image; message=%q", r.Message)
	}
	if strings.Contains(r.Message, "same binary") {
		t.Errorf("Message = %q, must not claim the two are one binary from bytes it never established belong to the process", r.Message)
	}
	if !strings.Contains(r.Message, "no longer the file at") {
		t.Errorf("Message = %q, want the unreachable-image verdict: the handle does not reach the running bytes", r.Message)
	}
}

// TestExecutedFacts_ContentHandleMustReachTheRunningInode is the same property
// at the seam that owns it, without the verdict machinery in the way.
func TestExecutedFacts_ContentHandleMustReachTheRunningInode(t *testing.T) {
	dir := t.TempDir()
	running := writeFakeBinary(t, filepath.Join(dir, "running"), "running bytes")
	img := imageAt(t, running)
	other := writeFakeBinary(t, filepath.Join(dir, "other"), "some other file entirely")

	img.contentPath = other
	if _, _, reachable := executedFacts(img, imagePathReplaced); reachable {
		t.Error("executedFacts trusted a handle that reaches a different inode; the bytes it returns are not the process's")
	}

	// The real shape: a handle that does reach the executing inode. A hard
	// link is what /proc/<pid>/exe behaves like — a second route to the same
	// bytes that survives the original name.
	linked := filepath.Join(dir, "handle")
	if err := os.Link(running, linked); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}
	img.contentPath = linked
	f, _, reachable := executedFacts(img, imagePathReplaced)
	if !reachable {
		t.Fatal("executedFacts rejected a handle that does reach the executing inode")
	}
	if f.readPath() != linked {
		t.Errorf("readPath = %q, want the handle %q — the resolution is what loses the inode", f.readPath(), linked)
	}
	if f.describePath() != running {
		t.Errorf("describePath = %q, want the operator-facing path %q", f.describePath(), running)
	}
}

// TestBinaryDivergenceCheck_ExecutedInodeUnderAnotherNameIsOK covers the
// artifact reached by two hard links where the process loaded it through the
// name that was later removed. The check already holds both identities when it
// forms its verdict; warning "nothing on disk describes the running bytes"
// over the top of evidence that the PATH binary IS the running inode sends an
// operator to restart a supervisor that is already converged.
func TestBinaryDivergenceCheck_ExecutedInodeUnderAnotherNameIsOK(t *testing.T) {
	dir := t.TempDir()
	loaded := writeFakeBinary(t, filepath.Join(dir, "gc-1.4.1"), "the one artifact")
	onPath := filepath.Join(dir, "gc")
	if err := os.Link(loaded, onPath); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}
	executing := imageAt(t, loaded)
	if err := os.Remove(loaded); err != nil {
		t.Fatalf("removing the loaded name %s: %v", loaded, err)
	}

	r := divergenceCheck(4242, executing, onPath).Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK: the PATH binary is the executed inode; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "same binary") {
		t.Errorf("Message = %q, want it to report the two as one binary", r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q, want none: there is nothing for the operator to converge", r.FixHint)
	}
}

// TestBinaryDivergenceCheck_UnlinkedInodeUnderAnotherNameIsNotOK is the guard
// on the exemption above. When the platform reported the image unlinked the
// kernel has already dropped that inode, so a file wearing its number is inode
// reuse, not the running artifact.
func TestBinaryDivergenceCheck_UnlinkedInodeUnderAnotherNameIsNotOK(t *testing.T) {
	dir := t.TempDir()
	path := writeFakeBinary(t, filepath.Join(dir, "gc"), "bytes")
	executing := imageAt(t, path)
	executing.unlinked = true

	r := divergenceCheck(4242, executing, path).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning: a matching inode for an unlinked image is reuse; message=%q", r.Status, r.Message)
	}
	if strings.Contains(r.Message, "same binary") {
		t.Errorf("Message = %q, must not read inode reuse as identity", r.Message)
	}
}

// TestReportUnreachableImage_IntactPathIsNotCalledAReplacement covers the
// TOCTOU half: executedFacts can fail to stat a path classifyImagePath
// observed intact moments earlier. "That path was replaced or removed under
// it" is then a state the check never established.
func TestReportUnreachableImage_IntactPathIsNotCalledAReplacement(t *testing.T) {
	dir := t.TempDir()
	path := writeFakeBinary(t, filepath.Join(dir, "gc-artifact"), "running")
	verifiedPath := writeFakeBinary(t, filepath.Join(dir, "gc"), "on path")
	c := divergenceCheck(4242, imageAt(t, path), verifiedPath)

	r := c.reportUnreachableImage(&CheckResult{Name: c.Name()}, imageAt(t, path), imagePathIntact,
		statBinary(verifiedPath), verifiedPath)

	if strings.Contains(r.Message, "replaced or removed") {
		t.Errorf("Message = %q, must not assert a replacement of a path it observed intact", r.Message)
	}
	if !strings.Contains(r.Message, path) {
		t.Errorf("Message = %q, want it to still name the image path %s", r.Message, path)
	}

	replaced := c.reportUnreachableImage(&CheckResult{Name: c.Name()}, imageAt(t, path), imagePathReplaced,
		statBinary(verifiedPath), verifiedPath)
	if !strings.Contains(replaced.Message, "no longer the file at") {
		t.Errorf("Message = %q, want the replaced wording when the path really was replaced", replaced.Message)
	}
}

// TestBinaryDivergenceCheck_PathLookupFailureIsNotAClaimAboutPath separates
// "gc is absent from PATH" — the only lookup outcome that is a fact about the
// operator's PATH — from "the lookup did not answer". Reporting the second as
// the first is a green ✓ asserting something never observed.
func TestBinaryDivergenceCheck_PathLookupFailureIsNotAClaimAboutPath(t *testing.T) {
	dir := t.TempDir()
	executed := writeFakeBinary(t, filepath.Join(dir, "gc"), "running")

	t.Run("not found is OK", func(t *testing.T) {
		c := divergenceCheck(5, imageAt(t, executed), "")
		c.lookPath = func(name string) (string, error) {
			return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
		}

		r := c.Run(&CheckContext{})

		if r.Status != StatusOK {
			t.Fatalf("Status = %v, want StatusOK when gc is genuinely absent; message=%q", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "not on PATH") {
			t.Errorf("Message = %q, want it to note the missing PATH entry", r.Message)
		}
	})

	t.Run("any other lookup failure is unverified", func(t *testing.T) {
		c := divergenceCheck(5, imageAt(t, executed), "")
		c.lookPath = func(name string) (string, error) {
			return "", &exec.Error{Name: name, Err: fs.ErrPermission}
		}

		r := c.Run(&CheckContext{})

		if r.Status != StatusWarning {
			t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "unverified") {
			t.Errorf("Message = %q, want it reported as unverified", r.Message)
		}
		if strings.Contains(r.Message, "not on PATH") {
			t.Errorf("Message = %q, must not claim gc is absent from PATH on a lookup that never answered", r.Message)
		}
	})

	t.Run("no lookup configured is unverified", func(t *testing.T) {
		c := divergenceCheck(5, imageAt(t, executed), "")
		c.lookPath = nil

		r := c.Run(&CheckContext{})

		if r.Status != StatusWarning {
			t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
		}
		if strings.Contains(r.Message, "not on PATH") {
			t.Errorf("Message = %q: a check with no PATH lookup at all knows nothing about PATH", r.Message)
		}
	})

	t.Run("a hit alongside ErrDot is still compared", func(t *testing.T) {
		// exec.LookPath returns both a usable path and exec.ErrDot when the
		// hit came from a relative PATH element. That binary is the one a
		// probe would execute, so it is the one to compare against.
		c := divergenceCheck(5, imageAt(t, executed), "")
		c.lookPath = func(name string) (string, error) {
			return executed, &exec.Error{Name: name, Err: exec.ErrDot}
		}

		r := c.Run(&CheckContext{})

		if r.Status != StatusOK {
			t.Fatalf("Status = %v, want StatusOK: the hit is the executed image itself; message=%q", r.Status, r.Message)
		}
		if strings.Contains(r.Message, "not on PATH") {
			t.Errorf("Message = %q, must not discard a usable hit because an error came with it", r.Message)
		}
	})
}

// TestBinaryDivergenceCheck_UnknownSupervisorLivenessIsUnverified covers the
// state a bare pid cannot carry. A control socket that accepted a connection
// and then failed to answer returns 0 from the liveness probe, and 0 is the
// one value that licenses a green verdict here — so a supervisor that is up,
// executing a stale image and wedged reports healthy. That is the exact state
// an operator runs `gc doctor` to find.
func TestBinaryDivergenceCheck_UnknownSupervisorLivenessIsUnverified(t *testing.T) {
	r := NewBinaryDivergenceCheck(SupervisorPIDUnknown).Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "unverified") {
		t.Errorf("Message = %q, want it reported as unverified", r.Message)
	}
	if strings.Contains(r.Message, "supervisor not running") {
		t.Errorf("Message = %q, must not assert the supervisor is absent from a probe that did not answer", r.Message)
	}

	// The settled negative is still a green check.
	if got := NewBinaryDivergenceCheck(0).Run(&CheckContext{}); got.Status != StatusOK {
		t.Errorf("Status = %v for a probe that established no supervisor is running, want StatusOK; message=%q", got.Status, got.Message)
	}
}

// TestTriStateZeroValuesAreTheUnknownState pins the shape rather than a path
// through it. Both enums used to put the permissive answer at iota-zero, so an
// unset contentOutcome meant "identical bytes" and an unset imagePathState
// meant "the path still names the running inode" — every accidental zero in
// this file was the green answer.
func TestTriStateZeroValuesAreTheUnknownState(t *testing.T) {
	var outcome contentOutcome
	if outcome != contentUnknown {
		t.Errorf("the zero contentOutcome is %v, want contentUnknown", outcome)
	}
	var state imagePathState
	if state != imagePathUnknown {
		t.Errorf("the zero imagePathState is %v, want imagePathUnknown", state)
	}
}

// TestCompareFailureNamesAnUnclassifiedOutcome covers the verdict switch's
// default arm. It is unreachable from any producer today; the arm exists
// because the arm that used to be there formed the DIVERGENCE verdict, so the
// next contributor to add a contentOutcome without a case would have shipped
// this file's defect for the fourth time.
func TestCompareFailureNamesAnUnclassifiedOutcome(t *testing.T) {
	read := fmt.Errorf("permission denied")
	if got := compareFailure(contentUnknown, read); !errors.Is(got, read) {
		t.Errorf("compareFailure = %v, want the read error that explains it", got)
	}
	got := compareFailure(contentOutcome(99), nil)
	if got == nil {
		t.Fatal("compareFailure on an unclassified outcome = nil; the message would read \"<nil>\"")
	}
	if !strings.Contains(got.Error(), "unclassified") {
		t.Errorf("error = %q, want it to say the outcome had no verdict rather than borrow a read failure's wording", got)
	}
}

// TestSelectRunningImage_AmbiguousUnevaluatableEntriesAreAnError is the guard
// for pass 2's tie-break. When an installer replaces the whole tree, the
// executable and its libraries are all unevaluatable together, and nothing
// distinguishes them — so lsof's ordering decided which one got named as the
// executed image, with a restart-the-supervisor hint attached. The resolver's
// own comment says naming a library that way is worse than saying nothing.
func TestSelectRunningImage_AmbiguousUnevaluatableEntriesAreAnError(t *testing.T) {
	dir := t.TempDir()
	lib := writeFakeBinary(t, filepath.Join(dir, "libicuuc.78.3.dylib"), "library")
	image := writeFakeBinary(t, filepath.Join(dir, "gc-main-20260904"), "the running image")
	entries := []lsofEntry{
		{name: lib, dev: fmt.Sprintf("%#x", identityOf(t, lib).dev), ino: fmt.Sprint(identityOf(t, lib).ino)},
		{name: image, dev: fmt.Sprintf("%#x", identityOf(t, image).dev), ino: fmt.Sprint(identityOf(t, image).ino)},
	}
	// The whole install tree replaced: every entry is now a different inode
	// than the one lsof recorded, the library ordered first.
	for _, p := range []string{lib, image} {
		replacement := writeFakeBinary(t, p+".new", "a different build")
		if err := os.Rename(replacement, p); err != nil {
			t.Fatalf("replacing %s: %v", p, err)
		}
	}

	got, err := selectRunningImage(entries, func(string) bool { return false })

	if err == nil {
		t.Fatalf("selectRunningImage named %q as the executed image on lsof's ordering alone; want an error", got.path)
	}
	if !strings.Contains(err.Error(), "cannot be told") {
		t.Errorf("error = %q, want it to say which entry is the image could not be determined", err)
	}
}

// TestExecutedFacts_IntactPathMustStillBeTheRunningInode closes F1's class on
// the branch the finding did not name.
//
// classifyImagePath establishes the path's identity at one stat; executedFacts
// then reads the bytes after a SECOND stat. A replacement landing between the
// two hands compareContent the replacement's bytes under the running image's
// name, and when those bytes match PATH — which after an in-place install is
// the normal case — the check answers "the same binary" for a fleet running
// the original inode. Identical failure to F1, sibling branch.
//
// The race cannot be lost on purpose, so the state is injected at the seam
// rather than timed: imagePathIntact is passed for a path that no longer names
// running.id, which is exactly what losing the race produces. The assertion is
// on the verdict, not on the window.
func TestExecutedFacts_IntactPathMustStillBeTheRunningInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gc-artifact")
	writeFakeBinary(t, path, "the bytes the fleet is running")
	running := imageAt(t, path)

	f, _, reachable := executedFacts(running, imagePathIntact)
	if !reachable {
		t.Fatal("executedFacts rejected an intact path that still names the running inode")
	}
	if f.readPath() == "" {
		t.Fatal("executedFacts returned no path to read the running bytes from")
	}

	replacement := writeFakeBinary(t, filepath.Join(dir, "gc-artifact.new"), "a different build")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replacing %s: %v", path, err)
	}

	if _, _, reachable := executedFacts(running, imagePathIntact); reachable {
		t.Error("executedFacts returned bytes from a file that is no longer the running inode; the verdict formed from them would describe the wrong artifact")
	}
}

// TestReportUnreachableImage_EveryStringMatchesTheState pins all three
// operator-facing fields on both routes.
//
// For `gc doctor` the strings ARE the product: an operator sees Status only as
// a color, so a Message, Details entry or FixHint asserting a state the check
// disproved is a verdict asserting something it did not establish, in the only
// sense that reaches a human. Details drifted from Message because Details was
// unasserted; FixHint then drifted from both because FixHint was unasserted.
// Assert all three, on both routes, or the next one drifts too.
func TestReportUnreachableImage_EveryStringMatchesTheState(t *testing.T) {
	dir := t.TempDir()
	path := writeFakeBinary(t, filepath.Join(dir, "gc-artifact"), "running")
	verifiedPath := writeFakeBinary(t, filepath.Join(dir, "gc"), "on path")
	c := divergenceCheck(4242, imageAt(t, path), verifiedPath)

	for name, tc := range map[string]struct {
		state       imagePathState
		wantAbsent  []string
		wantPresent []string
	}{
		// The file is still there and was not read — a root-owned 0111
		// artifact, say. Telling the operator to restart is advice that
		// changes nothing, over a Message saying the file is right there.
		"intact but unreadable": {
			state:       imagePathIntact,
			wantAbsent:  []string{"replaced or removed", "reachable only from inside the running process", "restart the supervisor"},
			wantPresent: []string{path},
		},
		"replaced under the process": {
			state:       imagePathReplaced,
			wantAbsent:  []string{},
			wantPresent: []string{"no longer the file at", "reachable only from inside the running process", "restart the supervisor"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := c.reportUnreachableImage(&CheckResult{Name: c.Name()}, imageAt(t, path), tc.state,
				statBinary(verifiedPath), verifiedPath)
			all := strings.Join(append(append([]string{r.Message}, r.Details...), r.FixHint), "\n")

			for _, want := range tc.wantPresent {
				if !strings.Contains(all, want) {
					t.Errorf("Message/Details/FixHint = %q, want it to contain %q", all, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(all, absent) {
					t.Errorf("Message/Details/FixHint = %q, must not contain %q for this state", all, absent)
				}
			}
			if r.FixHint == "" {
				t.Error("FixHint is empty; every unreachable report owes the operator something to do")
			}
		})
	}
}

// TestExecutedFacts_ReturnsTheStateItObserved is the fix for both wording
// findings at their source.
//
// classifyImagePath's answer is stale by the time executedFacts takes its own
// stat, and every string reportUnreachableImage prints is a claim about the
// state of the world. Passing the stale state made the wording describe a
// world the caller had just disproved: reaching the unreachable report at all
// with imagePathIntact requires running.id != verifiedID, and on the
// identity-mismatch route every clause of the intact wording is false.
//
// So executedFacts returns what it OBSERVED, and the wording is written from
// that. This also composes the join that could not be reached through Run —
// classifyImagePath returns imagePathIntact only when the identities agree, so
// Run can never present intact-plus-mismatch, but executedFacts' own boundary
// can.
func TestExecutedFacts_ReturnsTheStateItObserved(t *testing.T) {
	t.Run("intact and still the running inode", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFakeBinary(t, filepath.Join(dir, "gc"), "running")

		_, observed, reachable := executedFacts(imageAt(t, path), imagePathIntact)

		if !reachable {
			t.Fatal("reachable = false for a path that still names the running inode")
		}
		if observed != imagePathIntact {
			t.Errorf("observed = %v, want imagePathIntact", observed)
		}
	})

	t.Run("replaced between the two stats", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFakeBinary(t, filepath.Join(dir, "gc"), "running")
		running := imageAt(t, path)
		replacement := writeFakeBinary(t, filepath.Join(dir, "gc.new"), "a different build")
		if err := os.Rename(replacement, path); err != nil {
			t.Fatalf("replacing %s: %v", path, err)
		}

		_, observed, reachable := executedFacts(running, imagePathIntact)

		if reachable {
			t.Fatal("reachable = true for a path that no longer names the running inode")
		}
		if observed != imagePathReplaced {
			t.Errorf("observed = %v, want imagePathReplaced: the path was replaced, whatever classifyImagePath saw a moment earlier", observed)
		}
	})

	t.Run("removed between the two stats", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFakeBinary(t, filepath.Join(dir, "gc"), "running")
		running := imageAt(t, path)
		if err := os.Remove(path); err != nil {
			t.Fatalf("removing %s: %v", path, err)
		}

		_, observed, reachable := executedFacts(running, imagePathIntact)

		if reachable {
			t.Fatal("reachable = true for a path that is gone")
		}
		if observed != imagePathReplaced {
			t.Errorf("observed = %v, want imagePathReplaced", observed)
		}
	})

	t.Run("still there but could not be looked at", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		dir := t.TempDir()
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		path := writeFakeBinary(t, filepath.Join(locked, "gc"), "running")
		running := imageAt(t, path)
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

		_, observed, reachable := executedFacts(running, imagePathIntact)

		if reachable {
			t.Fatal("reachable = true for a path that could not be stat'd")
		}
		if observed != imagePathIntact {
			t.Errorf("observed = %v, want imagePathIntact: a stat that was refused is not evidence of a replacement", observed)
		}
	})

	t.Run("replaced with no handle on the executing inode", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFakeBinary(t, filepath.Join(dir, "gc"), "running")
		running := imageAt(t, path) // contentPath deliberately empty: macOS

		_, observed, reachable := executedFacts(running, imagePathReplaced)

		if reachable {
			t.Fatal("reachable = true with no handle on the executing inode")
		}
		if observed != imagePathReplaced {
			t.Errorf("observed = %v, want imagePathReplaced", observed)
		}
	})
}

// TestReportUnreachableImage_WordsTheStateExecutedFactsObserved composes the
// two halves: the seam that observes the state, and the strings written from
// it. A replacement landing between classifyImagePath and executedFacts must
// produce the REPLACED wording — "no longer the file at", the unreachable
// Details, the restart hint — not the intact wording that says the file is
// still there.
func TestReportUnreachableImage_WordsTheStateExecutedFactsObserved(t *testing.T) {
	dir := t.TempDir()
	path := writeFakeBinary(t, filepath.Join(dir, "gc-artifact"), "the bytes the fleet is running")
	running := imageAt(t, path)
	verifiedPath := writeFakeBinary(t, filepath.Join(dir, "gc"), "the build on PATH")
	replacement := writeFakeBinary(t, filepath.Join(dir, "gc-artifact.new"), "a different build")
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replacing %s: %v", path, err)
	}

	c := divergenceCheck(4242, running, verifiedPath)
	_, observed, reachable := executedFacts(running, imagePathIntact)
	if reachable {
		t.Fatal("precondition: executedFacts must refuse a path that no longer names the running inode")
	}

	r := c.reportUnreachableImage(&CheckResult{Name: c.Name()}, running, observed, statBinary(verifiedPath), verifiedPath)
	all := strings.Join(append(append([]string{r.Message}, r.Details...), r.FixHint), "\n")

	if !strings.Contains(r.Message, "no longer the file at") {
		t.Errorf("Message = %q, want the replaced wording: the path really was replaced", r.Message)
	}
	if strings.Contains(all, "is still the file it was loaded from") {
		t.Errorf("output = %q, must not say the file is still there about one that was replaced", all)
	}
	if !strings.Contains(r.FixHint, "restart the supervisor") {
		t.Errorf("FixHint = %q, want the restart advice for an artifact that is gone", r.FixHint)
	}
}

// TestBinaryDivergenceCheck_AmbiguousMappedTextSetIsUnverifiedEndToEnd pins
// the verdict an ambiguous lsof set produces through Run, not just the error
// selectRunningImage returns. An installer that replaced the whole tree leaves
// every entry unevaluatable; naming one of them would attach a
// restart-the-supervisor hint to a shared library.
func TestBinaryDivergenceCheck_AmbiguousMappedTextSetIsUnverifiedEndToEnd(t *testing.T) {
	dir := t.TempDir()
	lib := writeFakeBinary(t, filepath.Join(dir, "libicuuc.78.3.dylib"), "library")
	image := writeFakeBinary(t, filepath.Join(dir, "gc-main-20260904"), "the running image")
	entries := []lsofEntry{
		{name: lib, dev: fmt.Sprintf("%#x", identityOf(t, lib).dev), ino: fmt.Sprint(identityOf(t, lib).ino)},
		{name: image, dev: fmt.Sprintf("%#x", identityOf(t, image).dev), ino: fmt.Sprint(identityOf(t, image).ino)},
	}
	for _, p := range []string{lib, image} {
		replacement := writeFakeBinary(t, p+".new", "a different build")
		if err := os.Rename(replacement, p); err != nil {
			t.Fatalf("replacing %s: %v", p, err)
		}
	}
	verified := writeFakeBinary(t, filepath.Join(dir, "gc"), "the build on PATH")

	c := divergenceCheck(4242, runningImage{}, verified)
	c.resolveRunningImage = func(int) (runningImage, error) {
		return selectRunningImage(entries, func(string) bool { return false })
	}

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "unverified") {
		t.Errorf("Message = %q, want it reported as unverified", r.Message)
	}
	if strings.Contains(r.Message, "binary divergence: ") {
		t.Errorf("Message = %q, must not assert a divergence about an image it could not identify", r.Message)
	}
	if strings.Contains(r.Message, "no longer the file at") {
		t.Errorf("Message = %q, must not report an unreachable image it never picked out of the set", r.Message)
	}
	if strings.Contains(r.FixHint, "restart the supervisor") {
		t.Errorf("FixHint = %q, must not tell the operator to restart over a library", r.FixHint)
	}
}

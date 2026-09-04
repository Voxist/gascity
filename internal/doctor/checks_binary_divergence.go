package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

// gcCommandName is the PATH name a probe resolves when it shells out to
// "gc ...". It is the verification target this check compares the running
// supervisor's executed image against.
const gcCommandName = "gc"

// binaryDivergenceVersionTimeout bounds each `<binary> version` invocation so
// a wedged or non-gc binary on either side cannot stall the check.
const binaryDivergenceVersionTimeout = 5 * time.Second

// deletedImageSuffix is the marker the kernel appends to /proc/<pid>/exe (and
// lsof reports) when the executed image has been unlinked or replaced on disk.
const deletedImageSuffix = " (deleted)"

// BinaryDivergenceCheck detects the case where the gc binary a probe verifies
// is not the gc binary the supervisor is executing.
//
// The hazard is silent and universal to any gc deployment: a service unit
// references one path, an operator's PATH resolves another, and nothing
// asserts the two are the same bytes. When they diverge, a capability probe
// run from a shell ("does this build apply Order.Env?") describes a build the
// supervisor is not running, so a feature can verify green and be absent at
// dispatch — or the reverse. Direction matters as much as difference: an
// operator needs to know whether they are verifying ahead of or behind the
// fleet, so a divergent result names both paths, both versions and both
// modification times.
//
// The executed image is read from the process, not from the service unit's
// path string. That distinction is the point: re-pointing a symlink does not
// re-exec a running process, so resolving the unit's path would report the
// artifact the symlink names *now* rather than the inode the supervisor is
// actually executing.
type BinaryDivergenceCheck struct {
	// supervisorPID is the PID of the running supervisor, or 0 when none is
	// running. Sourced from the same control-socket probe the rest of the
	// doctor run uses.
	supervisorPID int
	// goos names the host platform, reported when no resolver exists for it.
	goos string
	// resolveRunningImage returns the on-disk path of the image a process is
	// executing. Nil when the host platform offers no route to it.
	resolveRunningImage func(pid int) (string, error)
	// lookPath resolves a command name against PATH.
	lookPath func(string) (string, error)
	// versionOf reports a binary's self-declared version, or "" when it
	// cannot be obtained.
	versionOf func(path string) string
}

// NewBinaryDivergenceCheck returns a check that compares the image the running
// supervisor is executing against the gc binary on PATH. supervisorPID should
// come from the supervisor liveness probe; pass 0 when none is running.
func NewBinaryDivergenceCheck(supervisorPID int) *BinaryDivergenceCheck {
	return &BinaryDivergenceCheck{
		supervisorPID:       supervisorPID,
		goos:                goruntime.GOOS,
		resolveRunningImage: runningImageResolverFor(goruntime.GOOS),
		lookPath:            exec.LookPath,
		versionOf:           binaryVersion,
	}
}

// Name returns the check identifier.
func (c *BinaryDivergenceCheck) Name() string { return "binary-divergence" }

// CanFix reports that this check does not support automatic remediation:
// choosing which artifact is canonical is an operator decision.
func (c *BinaryDivergenceCheck) CanFix() bool { return false }

// Fix is a no-op; CanFix returns false.
func (c *BinaryDivergenceCheck) Fix(_ *CheckContext) error { return nil }

// Run compares the supervisor's executed image with the PATH-resolved gc.
func (c *BinaryDivergenceCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	if c.supervisorPID <= 0 {
		r.Status = StatusOK
		r.Message = "supervisor not running — no executed image to compare"
		return r
	}
	if c.resolveRunningImage == nil {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("cannot read a process's executed image on %s — binary divergence NOT checked on this platform", c.goos)
		return r
	}

	running, err := c.resolveRunningImage(c.supervisorPID)
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("cannot resolve the image supervisor pid %d is executing: %v", c.supervisorPID, err)
		r.FixHint = "binary divergence stays unverified until the executed image can be read; confirm the supervisor process is alive and readable by this user"
		return r
	}
	if unlinked, deleted := strings.CutSuffix(running, deletedImageSuffix); deleted {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("supervisor pid %d is executing a deleted image (%s)", c.supervisorPID, unlinked)
		r.FixHint = "the artifact the supervisor is executing was replaced or removed on disk, so nothing on disk describes the running bytes; restart the supervisor so it re-executes the artifact now in place"
		return r
	}

	verified, err := c.resolvePathBinary()
	if err != nil {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("%s is not on PATH — nothing verifies against it (supervisor is executing %s)", gcCommandName, running)
		return r
	}

	runningFacts := statBinary(running)
	verifiedFacts := statBinary(verified)

	if same, reason := sameArtifact(runningFacts, verifiedFacts); same {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("supervisor and PATH %s are the same binary (%s)", gcCommandName, reason)
		r.Details = []string{
			fmt.Sprintf("executed by supervisor pid %d: %s", c.supervisorPID, runningFacts.describe()),
			fmt.Sprintf("resolved on PATH: %s", verifiedFacts.describe()),
		}
		return r
	}

	runningFacts.version = c.version(running)
	verifiedFacts.version = c.version(verified)

	r.Status = StatusWarning
	// The skew belongs in the message, not the details: which side is newer is
	// what an operator acts on, and details print only under --verbose.
	r.Message = fmt.Sprintf("binary divergence: supervisor pid %d is executing %s but PATH %s resolves to %s; %s",
		c.supervisorPID, runningFacts.realPath, gcCommandName, verifiedFacts.realPath,
		skewLine(runningFacts, verifiedFacts))
	r.Details = []string{
		fmt.Sprintf("executed (what the fleet runs): %s", runningFacts.describeVerbose()),
		fmt.Sprintf("verified (what probes hit): %s", verifiedFacts.describeVerbose()),
	}
	r.FixHint = fmt.Sprintf("any capability probe run through PATH %s describes a build the supervisor is not executing; converge the two on one artifact by re-pointing whichever path is stale, then restart the supervisor so it re-executes it — re-pointing a symlink alone does not change a running process's image", gcCommandName)
	return r
}

// version returns the binary's self-declared version, or "" when versionOf is
// unset or the binary does not answer.
func (c *BinaryDivergenceCheck) version(path string) string {
	if c.versionOf == nil {
		return ""
	}
	return c.versionOf(path)
}

// resolvePathBinary returns the absolute path PATH resolves gc to.
func (c *BinaryDivergenceCheck) resolvePathBinary() (string, error) {
	if c.lookPath == nil {
		return "", fmt.Errorf("no PATH lookup configured")
	}
	found, err := c.lookPath(gcCommandName)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(found)
	if err != nil {
		return found, nil //nolint:nilerr // a relative hit is still comparable
	}
	return abs, nil
}

// binaryFacts holds everything the check reports about one binary.
type binaryFacts struct {
	// path is the path as named by its discovery route: a PATH entry, or a
	// process's executed image.
	path string
	// realPath is path with symlinks resolved, or path when resolution fails.
	realPath string
	// info is the stat of realPath; nil when it could not be stat'd.
	info os.FileInfo
	// version is the binary's self-declared version. Populated only on the
	// divergent path, the one place it is worth running a binary to learn.
	version string
}

// statBinary resolves and stats one binary, tolerating failures so the check
// still reports the paths it does know.
func statBinary(path string) binaryFacts {
	f := binaryFacts{path: path, realPath: path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		f.realPath = resolved
	}
	if info, err := os.Stat(f.realPath); err == nil {
		f.info = info
	}
	return f
}

// describe renders the short form used when the two sides agree.
func (f binaryFacts) describe() string {
	if f.path == f.realPath {
		return f.path
	}
	return fmt.Sprintf("%s -> %s", f.path, f.realPath)
}

// describeVerbose renders the long form used when the two sides diverge:
// enough for an operator to tell which side is newer without another probe.
func (f binaryFacts) describeVerbose() string {
	var b strings.Builder
	b.WriteString(f.describe())
	if f.version != "" {
		fmt.Fprintf(&b, " version=%s", f.version)
	}
	if f.info != nil {
		fmt.Fprintf(&b, " modified=%s size=%d", f.info.ModTime().UTC().Format(time.RFC3339), f.info.Size())
	}
	return b.String()
}

// skewLine states which side is newer, which is what tells an operator whether
// they are verifying ahead of or behind the running fleet.
func skewLine(running, verified binaryFacts) string {
	if running.info == nil || verified.info == nil {
		return "cannot compare build times: at least one binary could not be stat'd"
	}
	rt, vt := running.info.ModTime(), verified.info.ModTime()
	switch {
	case vt.After(rt):
		return fmt.Sprintf("the verified binary is NEWER by %s: probes are verifying ahead of the running fleet", vt.Sub(rt).Round(time.Second))
	case rt.After(vt):
		return fmt.Sprintf("the executed binary is NEWER by %s: probes are verifying behind the running fleet", rt.Sub(vt).Round(time.Second))
	default:
		return "both binaries share a modification time but differ in content"
	}
}

// sameArtifact reports whether two binaries are the same bytes, and why. Paths
// are compared first (the cheap, common case), then file identity, then
// content — two distinct paths that are symlinks to one artifact, or copies of
// identical bytes, are healthy and must not warn.
func sameArtifact(a, b binaryFacts) (bool, string) {
	if a.realPath == b.realPath {
		return true, a.realPath
	}
	if a.info != nil && b.info != nil && os.SameFile(a.info, b.info) {
		return true, fmt.Sprintf("%s and %s are the same file", a.realPath, b.realPath)
	}
	if a.info == nil || b.info == nil || a.info.Size() != b.info.Size() {
		return false, ""
	}
	sumA, errA := fileSHA256(a.realPath)
	sumB, errB := fileSHA256(b.realPath)
	if errA != nil || errB != nil || sumA != sumB {
		return false, ""
	}
	return true, fmt.Sprintf("%s and %s are different paths with identical content (sha256 %s)", a.realPath, b.realPath, sumA[:12])
}

// fileSHA256 returns the hex-encoded SHA-256 of a file's contents.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runningImageResolverFor returns the resolver for the host platform, or nil
// when the platform offers no route to a process's executed image.
func runningImageResolverFor(goos string) func(pid int) (string, error) {
	switch goos {
	case "linux":
		return func(pid int) (string, error) { return runningImageFromProc("/proc", pid) }
	case "darwin":
		return runningImageFromLsof
	default:
		return nil
	}
}

// runningImageFromProc reads /proc/<pid>/exe, the kernel's own record of the
// image a process is executing. procRoot is a parameter so the read is
// testable on hosts without procfs.
func runningImageFromProc(procRoot string, pid int) (string, error) {
	link := filepath.Join(procRoot, fmt.Sprint(pid), "exe")
	target, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", link, err)
	}
	if strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("reading %s: empty target", link)
	}
	return target, nil
}

// runningImageFromLsof reads the executed image from lsof's txt (mapped text)
// entries, which macOS offers in place of procfs.
func runningImageFromLsof(pid int) (string, error) {
	out, err := exec.Command("lsof", "-p", fmt.Sprint(pid), "-a", "-d", "txt", "-Fn").Output()
	if err != nil && len(out) == 0 {
		return "", fmt.Errorf("running lsof for pid %d: %w", pid, err)
	}
	return parseLsofTxtImage(out)
}

// parseLsofTxtImage extracts the executable from lsof -Fn -d txt output. The
// txt set holds the executable followed by the dynamic loader and every mapped
// library, so the first entry that is not a library is the image.
func parseLsofTxtImage(out []byte) (string, error) {
	for _, line := range strings.Split(string(bytes.TrimRight(out, "\n")), "\n") {
		name, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "n")
		if !ok || name == "" {
			continue
		}
		if isSharedLibraryPath(name) {
			continue
		}
		return name, nil
	}
	return "", fmt.Errorf("no executable image in lsof txt entries")
}

// isSharedLibraryPath reports whether a mapped txt entry is a library or the
// dynamic loader rather than the process's own executable.
func isSharedLibraryPath(path string) bool {
	base := filepath.Base(path)
	if base == "dyld" {
		return true
	}
	return strings.HasSuffix(base, ".dylib") || strings.HasSuffix(base, ".so") || strings.Contains(base, ".so.")
}

// binaryVersion runs `<path> version` under a timeout and returns its first
// output line. It returns "" when the binary does not answer, so a divergent
// result still reports the paths and modification times it does know.
func binaryVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), binaryDivergenceVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}

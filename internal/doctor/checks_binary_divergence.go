package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/macho"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
)

// gcCommandName is the PATH name a probe resolves when it shells out to
// "gc ...". It is the verification target this check compares the running
// supervisor's executed image against.
const gcCommandName = "gc"

// binaryDivergenceProbeTimeout bounds every subprocess this check runs — the
// lsof image probe and each `<binary> version` invocation — so a wedged binary
// or a stale network mount cannot stall the check. The doctor's own
// CheckTimeout only abandons the goroutine; it does not reap the process, so
// each subprocess has to bound itself.
const binaryDivergenceProbeTimeout = 5 * time.Second

// binaryDivergenceWaitDelay bounds how long a killed subprocess's inherited
// output pipes stay open. Without it, killing the child does not close stdout
// when a descendant inherited it, and Output() blocks past the deadline.
const binaryDivergenceWaitDelay = time.Second

// deletedImageSuffix is the marker Linux appends to /proc/<pid>/exe when the
// executed image has been unlinked or replaced on disk.
//
// macOS does NOT do this: lsof reports the original path with no marker after
// the file at that path has been replaced. That asymmetry is why this check
// compares file identity rather than the reported path — see
// BinaryDivergenceCheck.
const deletedImageSuffix = " (deleted)"

// fileIdentity is a file's kernel identity: the device/inode pair that stays
// with the bytes when the path is re-pointed or the file replaced underneath
// it. Comparing identities is what distinguishes "the same binary reached by
// two names" from "two different binaries that happen to share a name".
type fileIdentity struct {
	dev uint64
	ino uint64
}

// runningImage describes the image a process is executing.
type runningImage struct {
	// path is where the image was loaded from. It is a stale label, not an
	// identity: the file at that path may since have been replaced, which is
	// exactly the case this check exists to catch.
	path string
	// id identifies the executing inode itself.
	id fileIdentity
	// unlinked records that the platform reported the image as deleted. Only
	// Linux reports this; it is a display detail, not the signal — the
	// identity comparison catches the macOS case where nothing is reported.
	unlinked bool
}

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
// Two things make this check work where the obvious implementation does not.
//
// First, the executed image is read from the process, not from the path its
// service unit names. Re-pointing a symlink does not re-exec a running
// process, so resolving the unit's path string would report the artifact the
// symlink names *now* rather than the inode the supervisor is executing.
//
// Second, the comparison is on file identity, never on the path string. When
// an installer replaces the binary in place — the "last writer wins silently"
// case — the process keeps executing the old inode while its reported path is
// unchanged. Linux marks that image "(deleted)"; macOS does not mark it at
// all, so on macOS both path strings are identical while the bytes differ. A
// path-equality shortcut would report "same binary" for the very incident this
// check exists to catch, on the platform the incident happened on.
type BinaryDivergenceCheck struct {
	// supervisorPID is the PID of the running supervisor, or 0 when none is
	// running. Sourced from the same control-socket probe the rest of the
	// doctor run uses.
	supervisorPID int
	// goos names the host platform, reported when no resolver exists for it.
	goos string
	// resolveRunningImage returns the image a process is executing. Nil when
	// the host platform offers no route to it.
	resolveRunningImage func(pid int) (runningImage, error)
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
		// Reported as unverified, not OK. "The comparison did not happen" is
		// the same epistemic state whether the cause is a missing platform
		// route or a failed stat, and a green check for a comparison that
		// never ran is the exact failure this check exists to surface one
		// level up.
		return unverified(r, "no way to read a process's executed image on %s — NOT checked on this platform", c.goos)
	}

	running, err := c.resolveRunningImage(c.supervisorPID)
	if err != nil {
		return unverified(r, "cannot resolve the image supervisor pid %d is executing: %v", c.supervisorPID, err)
	}

	verifiedPath, err := c.resolvePathBinary()
	if err != nil {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("%s is not on PATH — nothing verifies against it (supervisor is executing %s)", gcCommandName, running.path)
		return r
	}

	verified := statBinary(verifiedPath)
	if verified.info == nil {
		return unverified(r, "cannot stat the PATH %s at %s", gcCommandName, verified.realPath)
	}
	verifiedID, ok := fileIdentityOf(verified.info)
	if !ok {
		return unverified(r, "this filesystem exposes no file identity for %s, so the executed image cannot be compared to it", verified.realPath)
	}

	// Whether any file on disk is still the running image is settled before
	// identity is compared. A platform that reports the image unlinked has said
	// outright that none is, and an inode number that matches after that is
	// inode reuse, not identity — the one way an identity comparison can still
	// produce a false agreement.
	switch classifyImagePath(running) {
	case imagePathUnknown:
		return unverified(r, "cannot tell whether %s is still the image supervisor pid %d is executing", running.path, c.supervisorPID)
	case imagePathReplaced:
		verified.version = c.version(verifiedPath)
		r.Status = StatusWarning
		r.Message = fmt.Sprintf(
			"binary divergence: supervisor pid %d is executing an image that is no longer the file at %s — that path was replaced or removed under it, and PATH %s resolves to %s",
			c.supervisorPID, running.path, gcCommandName, verified.realPath)
		r.Details = []string{
			fmt.Sprintf("executed (what the fleet runs): inode %d — the running bytes now exist only in the running process, so nothing on disk describes them", running.id.ino),
			fmt.Sprintf("verified (what probes hit): %s", verified.describeVerbose()),
		}
		r.FixHint = fmt.Sprintf("the artifact the supervisor is executing was replaced under it, so no capability probe can describe the running bytes; restart the supervisor so it re-executes the artifact now on disk, then re-run %s doctor", gcCommandName)
		return r
	}

	// The path names the running inode, so identity settles the comparison and
	// the on-disk reads below describe the running binary, not a successor.
	if running.id == verifiedID {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("supervisor and PATH %s are the same binary (%s)",
			gcCommandName, sameBinaryReason(running, verified))
		r.Details = []string{
			fmt.Sprintf("executed by supervisor pid %d: %s (inode %d)", c.supervisorPID, running.path, running.id.ino),
			fmt.Sprintf("resolved on PATH: %s", verified.describe()),
		}
		return r
	}

	executed := statBinary(running.path)

	// Two distinct files with identical bytes are healthy: the fleet runs what
	// the probe verified. Only reachable now that the path is confirmed to
	// still name the running inode.
	if same, reason := sameContent(executed, verified); same {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("supervisor and PATH %s are the same binary (%s)", gcCommandName, reason)
		r.Details = []string{
			fmt.Sprintf("executed by supervisor pid %d: %s", c.supervisorPID, executed.describe()),
			fmt.Sprintf("resolved on PATH: %s", verified.describe()),
		}
		return r
	}

	executed.version = c.version(running.path)
	verified.version = c.version(verifiedPath)

	// The skew belongs in the message, not the details: which side is newer is
	// what an operator acts on, and details print only under --verbose.
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("binary divergence: supervisor pid %d is executing %s but PATH %s resolves to %s; %s",
		c.supervisorPID, executed.realPath, gcCommandName, verified.realPath,
		skewLine(executed, verified))
	r.Details = []string{
		fmt.Sprintf("executed (what the fleet runs): %s", executed.describeVerbose()),
		fmt.Sprintf("verified (what probes hit): %s", verified.describeVerbose()),
	}
	r.FixHint = fmt.Sprintf("any capability probe run through PATH %s describes a build the supervisor is not executing; converge the two on one artifact by re-pointing whichever path is stale, then restart the supervisor so it re-executes it — re-pointing a symlink alone does not change a running process's image", gcCommandName)
	return r
}

// unverified reports that the check could not establish whether the two
// binaries agree. It is deliberately distinct from a divergence finding: a
// stat that failed is not evidence of a difference, and must not be dressed up
// as one.
func unverified(r *CheckResult, format string, args ...any) *CheckResult {
	r.Status = StatusWarning
	r.Message = "binary divergence unverified: " + fmt.Sprintf(format, args...)
	r.FixHint = "the executed and verified binaries could not be compared, so neither agreement nor divergence is established; re-run once the supervisor process and both binaries are readable by this user"
	return r
}

// sameBinaryReason describes why the two sides are the same binary, naming
// both routes when they are reached by different paths.
func sameBinaryReason(running runningImage, verified binaryFacts) string {
	if running.path == verified.realPath {
		return running.path
	}
	return fmt.Sprintf("%s and %s are one file, inode %d", running.path, verified.realPath, running.id.ino)
}

// imagePathState classifies whether the path a process was loaded from still
// refers to the inode it is executing.
type imagePathState int

const (
	// imagePathIntact means the path still names the running inode, so the
	// bytes on disk at that path are the bytes the process is running.
	imagePathIntact imagePathState = iota
	// imagePathReplaced means the artifact was replaced or removed underneath
	// the running process, so nothing readable on disk describes the running
	// bytes.
	imagePathReplaced
	// imagePathUnknown means the question could not be answered — a stat that
	// failed for a reason other than the file being gone. Distinct from
	// imagePathReplaced because a permission error is not evidence of a
	// replacement.
	imagePathUnknown
)

// classifyImagePath answers whether an executed image is still on disk where
// the process loaded it from.
func classifyImagePath(img runningImage) imagePathState {
	// An image the platform reports as unlinked is gone regardless of what
	// stands at its path now. Checking this first is what stops inode reuse —
	// the kernel handing a new file the number the old one had — from reading
	// as the running image.
	if img.unlinked {
		return imagePathReplaced
	}
	info, err := os.Stat(img.path)
	if err != nil {
		if os.IsNotExist(err) {
			return imagePathReplaced
		}
		return imagePathUnknown
	}
	id, ok := fileIdentityOf(info)
	if !ok {
		return imagePathUnknown
	}
	if id == img.id {
		return imagePathIntact
	}
	return imagePathReplaced
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

// binaryFacts holds everything the check reports about one binary on disk.
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

// contentChunkSize bounds both the tail sample and the streaming read buffer.
const contentChunkSize = 64 << 10

// sameContent reports whether two distinct files hold identical bytes, and
// why.
//
// Three gates, cheapest first. Size rules out most pairs for a stat. A bounded
// tail sample then rules out the one shape the streaming pass below would
// otherwise read two whole binaries to settle: same size, identical for most
// of their length, differing only near the end — which is where a Go build ID
// lives. What survives both is read in a single streaming pass that compares
// as it goes, stops at the first differing byte, and digests one side for the
// report.
//
// Proving two files identical does require reading both, so that cost is
// inherent to a positive answer; what it no longer costs is a second pass to
// hash the other side.
func sameContent(a, b binaryFacts) (bool, string) {
	if a.info == nil || b.info == nil || a.info.Size() != b.info.Size() {
		return false, ""
	}
	if same, err := sameTail(a.realPath, b.realPath, a.info.Size()); err != nil || !same {
		return false, ""
	}
	sum, err := compareAndDigest(a.realPath, b.realPath)
	if err != nil || sum == "" {
		return false, ""
	}
	return true, fmt.Sprintf("%s and %s are different files with identical content (sha256 %s)", a.realPath, b.realPath, sum[:12])
}

// sameTail compares a bounded sample from the end of two equally-sized files.
// Only the tail is sampled: a difference near the start is already settled by
// the streaming pass's first chunk, so a head sample would re-read bytes that
// comparison is about to read anyway.
func sameTail(pathA, pathB string, size int64) (bool, error) {
	n := int64(contentChunkSize)
	if n > size {
		n = size
	}
	if n == 0 {
		return true, nil
	}
	a, err := os.Open(pathA)
	if err != nil {
		return false, err
	}
	defer a.Close() //nolint:errcheck // read-only handle
	b, err := os.Open(pathB)
	if err != nil {
		return false, err
	}
	defer b.Close() //nolint:errcheck // read-only handle

	off := size - n
	bufA := make([]byte, n)
	bufB := make([]byte, n)
	if _, err := a.ReadAt(bufA, off); err != nil {
		return false, err
	}
	if _, err := b.ReadAt(bufB, off); err != nil {
		return false, err
	}
	return bytes.Equal(bufA, bufB), nil
}

// compareAndDigest streams both files once, comparing as it reads and
// digesting the first. It returns the hex-encoded SHA-256 when the contents
// match, and an empty string when they differ.
func compareAndDigest(pathA, pathB string) (string, error) {
	a, err := os.Open(pathA)
	if err != nil {
		return "", err
	}
	defer a.Close() //nolint:errcheck // read-only handle
	b, err := os.Open(pathB)
	if err != nil {
		return "", err
	}
	defer b.Close() //nolint:errcheck // read-only handle

	h := sha256.New()
	bufA := make([]byte, contentChunkSize)
	bufB := make([]byte, contentChunkSize)
	for {
		nA, errA := io.ReadFull(a, bufA)
		nB, errB := io.ReadFull(b, bufB)
		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return "", nil
		}
		h.Write(bufA[:nA]) //nolint:errcheck // hash.Write never errors
		atEndA := errA == io.EOF || errA == io.ErrUnexpectedEOF
		atEndB := errB == io.EOF || errB == io.ErrUnexpectedEOF
		if atEndA != atEndB {
			return "", nil
		}
		if atEndA {
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if errA != nil {
			return "", errA
		}
		if errB != nil {
			return "", errB
		}
	}
}

// runningImageResolverFor returns the resolver for the host platform, or nil
// when the platform offers no route to a process's executed image.
func runningImageResolverFor(goos string) func(pid int) (runningImage, error) {
	switch goos {
	case "linux":
		return func(pid int) (runningImage, error) { return runningImageFromProc("/proc", pid) }
	case "darwin":
		return runningImageFromLsof
	default:
		return nil
	}
}

// runningImageFromProc reads the executed image from /proc/<pid>/exe, the
// kernel's own record.
//
// os.Stat on the link — not os.Lstat, and not a stat of the path the link
// names — is required and load-bearing. /proc/<pid>/exe is a magic link:
// following it lands on the executing inode itself, even after that inode has
// been unlinked or the path re-pointed at a different file. os.Lstat would
// instead return the procfs pseudo-entry, whose device and inode belong to
// procfs and match no file on disk, turning every Linux run into a false
// divergence. Stat'ing the link's textual target would defeat the check the
// other way, by describing whatever now sits at that path.
//
// procRoot is a parameter so the read is testable on hosts without procfs.
func runningImageFromProc(procRoot string, pid int) (runningImage, error) {
	link := filepath.Join(procRoot, strconv.Itoa(pid), "exe")
	target, err := os.Readlink(link)
	if err != nil {
		return runningImage{}, fmt.Errorf("reading %s: %w", link, err)
	}
	img := runningImage{}
	img.path, img.unlinked = strings.CutSuffix(strings.TrimSpace(target), deletedImageSuffix)
	if img.path == "" {
		return runningImage{}, fmt.Errorf("reading %s: empty target", link)
	}
	info, err := os.Stat(link)
	if err != nil {
		return runningImage{}, fmt.Errorf("stat %s: %w", link, err)
	}
	id, ok := fileIdentityOf(info)
	if !ok {
		return runningImage{}, fmt.Errorf("stat %s: no file identity available", link)
	}
	img.id = id
	return img, nil
}

// runningImageFromLsof reads the executed image from lsof's txt (mapped text)
// entries, which macOS offers in place of procfs. -b -w keep lsof off blocking
// kernel calls (and silence the warnings that mode emits) so a stale network
// mount cannot wedge the probe.
func runningImageFromLsof(pid int) (runningImage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), binaryDivergenceProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-p", strconv.Itoa(pid), "-a", "-d", "txt", "-b", "-w", "-FnDi")
	cmd.WaitDelay = binaryDivergenceWaitDelay
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return runningImage{}, fmt.Errorf("running lsof for pid %d: %w", pid, err)
	}
	return selectRunningImage(parseLsofFileEntries(out), machOIsExecutable)
}

// lsofEntry is one file record from lsof -F output.
type lsofEntry struct {
	name string
	dev  string
	ino  string
}

// parseLsofFileEntries splits lsof -F output into per-file records. Fields
// accumulate into the current record until the next file ('f') or process
// ('p') marker.
func parseLsofFileEntries(out []byte) []lsofEntry {
	var entries []lsofEntry
	var cur *lsofEntry
	flush := func() {
		if cur != nil && cur.name != "" {
			entries = append(entries, *cur)
		}
		cur = nil
	}
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		field, value := line[0], line[1:]
		switch field {
		case 'p':
			flush()
		case 'f':
			flush()
			cur = &lsofEntry{}
		case 'n':
			if cur != nil {
				cur.name = value
			}
		case 'D':
			if cur != nil {
				cur.dev = value
			}
		case 'i':
			if cur != nil {
				cur.ino = value
			}
		}
	}
	flush()
	return entries
}

// selectRunningImage picks the executable out of a process's mapped-text set.
// The set holds the dynamic loader, every mapped library, and — for a binary
// that links ICU, as gc does — mapped data files such as locale tables and
// icudt*.dat. Only the file that is itself an executable qualifies, which is
// why this asks the file format rather than denylisting suffixes: a data file
// ordered ahead of the image would otherwise be named as the executed binary.
func selectRunningImage(entries []lsofEntry, isExecutable func(string) bool) (runningImage, error) {
	if len(entries) == 0 {
		return runningImage{}, fmt.Errorf("lsof reported no mapped-text entries")
	}
	for _, e := range entries {
		path, _ := strings.CutSuffix(e.name, deletedImageSuffix)
		if path == "" || !isExecutable(path) {
			continue
		}
		return imageFromLsofEntry(e)
	}
	// No entry is a readable Mach-O executable. That is not a reason to give
	// up: the commonest way to reach it is that the executed image was
	// deleted, so the read that asks "is this an executable?" fails on a file
	// that is no longer there. Skipping it would turn the sharpest finding
	// this check can make — the fleet is running bytes that exist nowhere on
	// disk — into "unverified", and send the operator looking for a
	// permissions problem. A path replaced by a non-Mach-O (a shell wrapper,
	// say) lands here too, and is equally a finding.
	//
	// Fall back to the entry the process was loaded from. macOS lists the
	// executable first in the mapped-text set, and classifyImagePath decides
	// what the entry means: gone from disk, or a different file than the one
	// running. Either way the answer is reported rather than withheld.
	return imageFromLsofEntry(entries[0])
}

// imageFromLsofEntry converts one mapped-text record into a runningImage.
func imageFromLsofEntry(e lsofEntry) (runningImage, error) {
	path, unlinked := strings.CutSuffix(e.name, deletedImageSuffix)
	if path == "" {
		return runningImage{}, fmt.Errorf("lsof reported a mapped-text entry with no name")
	}
	id, err := parseLsofIdentity(e)
	if err != nil {
		return runningImage{}, fmt.Errorf("executed image %s: %w", path, err)
	}
	return runningImage{path: path, id: id, unlinked: unlinked}, nil
}

// parseLsofIdentity converts one record's device (0x-prefixed hex) and inode
// (decimal) fields into a comparable identity. A missing or unparseable field
// is an error, never a silently dropped comparison.
func parseLsofIdentity(e lsofEntry) (fileIdentity, error) {
	rawDev := strings.TrimPrefix(strings.TrimSpace(e.dev), "0x")
	if rawDev == "" {
		return fileIdentity{}, fmt.Errorf("lsof reported no device number")
	}
	dev, err := strconv.ParseUint(rawDev, 16, 64)
	if err != nil {
		return fileIdentity{}, fmt.Errorf("parsing device number %q: %w", e.dev, err)
	}
	rawIno := strings.TrimSpace(e.ino)
	if rawIno == "" {
		return fileIdentity{}, fmt.Errorf("lsof reported no inode number")
	}
	ino, err := strconv.ParseUint(rawIno, 10, 64)
	if err != nil {
		return fileIdentity{}, fmt.Errorf("parsing inode number %q: %w", e.ino, err)
	}
	return fileIdentity{dev: dev, ino: ino}, nil
}

// machOIsExecutable reports whether a file is a Mach-O executable — not a
// dylib, not the dynamic loader, and not a mapped data file. Universal
// binaries qualify when any slice is an executable.
func machOIsExecutable(path string) bool {
	if f, err := macho.Open(path); err == nil {
		defer f.Close() //nolint:errcheck // read-only handle
		return f.Type == macho.TypeExec
	}
	fat, err := macho.OpenFat(path)
	if err != nil {
		return false
	}
	defer fat.Close() //nolint:errcheck // read-only handle
	for _, arch := range fat.Arches {
		if arch.Type == macho.TypeExec {
			return true
		}
	}
	return false
}

// binaryVersion runs `<path> version` under a timeout and returns its first
// output line. It returns "" when the binary does not answer, so a divergent
// result still reports the paths and modification times it does know.
func binaryVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), binaryDivergenceProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	// Without WaitDelay, killing the child does not close stdout when a
	// descendant inherited it, and Output() blocks past the deadline.
	cmd.WaitDelay = binaryDivergenceWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}

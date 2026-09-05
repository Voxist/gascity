package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/macho"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// contentPath, when non-empty, reads the executing inode's bytes even
	// after the file at path has been replaced or removed — Linux's
	// /proc/<pid>/exe is such a handle. macOS offers none, so a replaced
	// image's bytes are unreachable there and the check says so rather than
	// guessing at them.
	contentPath string
	// unlinked records that the platform reported the image as deleted. Only
	// Linux reports this; it is not the signal the check leans on, but it does
	// settle one case identity alone cannot — see classifyImagePath.
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
// Three rules shape the implementation, each earned by a way the obvious
// version gets the answer wrong.
//
// First, the executed image is read from the process, not from the path its
// service unit names. Re-pointing a symlink does not re-exec a running
// process, so resolving the unit's path string would report the artifact the
// symlink names *now* rather than the inode the supervisor is executing.
//
// Second, the comparison is on file identity, never on the path string. When
// an installer replaces the binary in place, the process keeps executing the
// old inode while its reported path is unchanged. Linux marks that image
// "(deleted)"; macOS does not mark it at all, so on macOS both path strings
// are identical while the bytes differ.
//
// Third — and this is the rule the other two keep re-teaching — every step
// that cannot obtain an answer reports that it could not, and never falls
// through to a verdict. A stat that failed, a binary that cannot be read, a
// platform with no route to a process image: none of those are evidence that
// two binaries differ, and a check built to catch verification that describes
// the wrong object must not itself assert what it did not observe. Every
// information-gathering step below therefore reports a tri-state. There are
// many places a verdict is formed — roughly a dozen and a half exits across
// Run and reportUnreachableImage — so the rule is a property every one of them
// must hold rather than a bottleneck one of them enforces.
//
// The rule covers two kinds of claim, and review rounds have now found both
// broken:
//
//   - claims about the COMPARISON: that the binaries agree, that they differ,
//     that no supervisor is running. Three rounds found exits asserting one
//     without the observation behind it, and a fourth found fact-gathering
//     steps feeding a sound exit a fact they had not established.
//   - claims about the STATE OF THE WORLD: that a file is still at its path,
//     that it was replaced, that its bytes have no name on disk — carried
//     almost entirely by Message, Details and FixHint. For a doctor check
//     those strings ARE the result; an operator sees Status only as a color.
//     A string asserting a state the check disproved is the same defect in the
//     only layer a human reads, and a fifth round found two.
//
// One window is known to remain open and is not closed here: the identity of
// the executed image is proved once, by stat, and compareContent then opens
// that path again (twice in sameTail, twice in compareAndDigest) and c.version
// execs it. A rename landing inside that window would let a contentSame
// verdict describe bytes the process is not running. It is a race rather than
// a class of world states — the bytes under a live executable cannot change in
// place, only by a rename that changes the inode — and closing it means
// opening the file once and stat'ing the descriptor rather than the path. See
// ga-7bcb8.
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

// SupervisorPIDUnknown is the supervisorPID a caller passes when its liveness
// probe did not settle the question — the control socket accepted a connection
// and then failed to answer, or answered unparseably.
//
// It exists because the caller's natural return type destroys the distinction
// this check depends on. A probe that times out and a supervisor that is not
// running both produce pid 0, and "no supervisor" is the one state that
// licenses a green verdict here. A supervisor that is up, executing a stale
// image, with a wedged control socket is exactly the state an operator runs
// `gc doctor` to find, and it must not be reported as healthy.
const SupervisorPIDUnknown = -1

// NewBinaryDivergenceCheck returns a check that compares the image the running
// supervisor is executing against the gc binary on PATH. supervisorPID should
// come from the supervisor liveness probe: a positive pid when one answered,
// 0 when the probe established that none is running, and SupervisorPIDUnknown
// when the probe could not tell.
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

	if c.supervisorPID < 0 {
		// A probe that did not answer has established nothing. Reporting the
		// "no supervisor" verdict here would be this check's own defect —
		// a verdict formed where no observation was made — one caller up.
		return unverified(r, "cannot tell whether a supervisor is running: the liveness probe did not settle it, so there is no executed image to compare")
	}
	if c.supervisorPID == 0 {
		r.Status = StatusOK
		r.Message = "supervisor not running — no executed image to compare"
		return r
	}
	if c.resolveRunningImage == nil {
		// Unverified, not OK. "The comparison did not happen" is the same
		// state whether the cause is a missing platform route or a failed
		// stat, and a green check for a comparison that never ran is the
		// exact failure this check exists to surface one level up.
		return unverified(r, "no way to read a process's executed image on %s — NOT checked on this platform", c.goos)
	}

	running, err := c.resolveRunningImage(c.supervisorPID)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			// Actionable, and a common deployment shape: a system-scope unit
			// running as root while doctor runs as the operator. "Re-run once
			// it is readable" would be advice they cannot act on.
			return unverifiedWithHint(r,
				fmt.Sprintf("supervisor pid %d runs as another user; re-run `%s doctor` as that user (or under sudo) to compare the binaries", c.supervisorPID, gcCommandName),
				"cannot read the image supervisor pid %d is executing: %v", c.supervisorPID, err)
		}
		return unverified(r, "cannot resolve the image supervisor pid %d is executing: %v", c.supervisorPID, err)
	}

	// Three outcomes, and only one of them is a statement about PATH.
	verifiedPath, err := c.resolvePathBinary()
	switch {
	case err == nil:
		// A hit. Fall through to the comparison below.
	case errors.Is(err, exec.ErrNotFound):
		// The only lookup failure that is a positive fact about PATH: gc is
		// genuinely absent, so nothing verifies against it.
		r.Status = StatusOK
		r.Message = fmt.Sprintf("%s is not on PATH — nothing verifies against it (supervisor is executing %s)", gcCommandName, running.path)
		return r
	default:
		// Every other failure means the lookup did not answer. "Not on PATH"
		// is a claim about the operator's PATH that this check has not
		// earned.
		return unverified(r, "cannot resolve %s on PATH: %v", gcCommandName, err)
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
	// identity is compared, because identity alone has one false agreement in
	// it: inode reuse. A platform that reports the image unlinked has said
	// outright that no file is the running image, so a matching inode number
	// after that is the kernel handing the number on, not the artifact.
	//
	// That is what running.unlinked gates, and it is the whole of the gate.
	// A merely REPLACED path is not disqualifying: the inode can survive the
	// name it was loaded from — a second hard link, or a rename out from
	// under the process — and identity still settles it correctly there. The
	// OK exit below requires imagePathIntact only because a replaced path
	// still has bytes worth comparing, so it takes the longer route; the
	// unreachable branch further down trusts identity on a replaced path
	// exactly when the platform did not report the image unlinked.
	state := classifyImagePath(running)
	if state == imagePathUnknown {
		return unverified(r, "cannot tell whether %s is still the image supervisor pid %d is executing", running.path, c.supervisorPID)
	}

	if state == imagePathIntact && running.id == verifiedID {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("supervisor and PATH %s are the same binary (%s)",
			gcCommandName, sameBinaryReason(running, verified))
		r.Details = []string{
			fmt.Sprintf("executed by supervisor pid %d: %s (inode %d)", c.supervisorPID, running.path, running.id.ino),
			fmt.Sprintf("resolved on PATH: %s", verified.describe()),
		}
		return r
	}

	// Identity says the two are different files. Whether that matters depends
	// on their bytes, which means reading the running image — reachable at its
	// own path while it is still there, and afterwards only through a platform
	// handle on the executing inode.
	// state is DELIBERATELY overwritten here rather than shadowed by a second
	// name. Everything below it — the wording, the details, the remediation —
	// is a claim about the state of the world, and the value classifyImagePath
	// produced above is stale the moment executedFacts takes its own stat. No
	// test can construct the disagreement (classifyImagePath returns intact
	// only while the identities agree, so Run cannot present intact-plus-
	// mismatch), which means no test can catch a line below that forwards the
	// stale value. Leaving no stale value to forward is the guard.
	executed, state, reachable := executedFacts(running, state)
	if !reachable {
		// The executed inode can outlive the name it was loaded from: a
		// second hard link, or a rename of the artifact out from under the
		// process. When the PATH binary IS that inode, the fleet is running
		// precisely what probes verify and there is nothing to converge —
		// evidence already in hand, and reporting "nothing on disk describes
		// the running bytes" over the top of it would send an operator to
		// restart a healthy supervisor.
		//
		// Not when the platform reported the image unlinked, though. The
		// kernel has already dropped that inode, so a file now wearing its
		// number is inode reuse, not identity — the same reason the OK gate
		// above requires imagePathIntact.
		if !running.unlinked && running.id == verifiedID {
			r.Status = StatusOK
			r.Message = fmt.Sprintf(
				"supervisor and PATH %s are the same binary — the executed inode %d is still on disk at %s, though the name it was loaded from (%s) is gone",
				gcCommandName, running.id.ino, verified.realPath, running.path)
			r.Details = []string{
				fmt.Sprintf("executed by supervisor pid %d: inode %d, loaded from %s", c.supervisorPID, running.id.ino, running.path),
				fmt.Sprintf("resolved on PATH: %s", verified.describe()),
			}
			return r
		}
		return c.reportUnreachableImage(r, running, state, verified, verifiedPath)
	}

	outcome, reason, err := compareContent(executed, verified)
	switch outcome {
	case contentDiffers:
		// The one outcome that licenses the divergence verdict formed below.
	case contentSame:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("supervisor and PATH %s are the same binary (%s)", gcCommandName, reason)
		r.Details = []string{
			fmt.Sprintf("executed by supervisor pid %d: %s", c.supervisorPID, executed.describe()),
			fmt.Sprintf("resolved on PATH: %s", verified.describe()),
		}
		if state == imagePathReplaced {
			r.Details = append(r.Details, fmt.Sprintf(
				"the file at %s was replaced since the supervisor started, but its bytes are unchanged — a restart would load the same build",
				running.path))
		}
		return r
	default:
		// contentUnknown, plus any outcome a later contributor adds without
		// giving it a verdict here. Both mean the same thing: the comparison
		// produced nothing this check may act on. Routing them to the
		// divergence verdict below — which is what an implicit fallthrough
		// would do — is the defect this file has now been corrected for
		// three times, pre-armed for the next enum value.
		return unverified(r, "cannot compare the contents of %s and %s: %v",
			executed.describePath(), verified.realPath, compareFailure(outcome, err))
	}

	executed.version = c.version(executed.readPath())
	verified.version = c.version(verifiedPath)

	// The skew belongs in the message, not the details: which side is newer is
	// what an operator acts on, and details print only under --verbose.
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("binary divergence: supervisor pid %d is executing %s but PATH %s resolves to %s; %s",
		c.supervisorPID, executed.describePath(), gcCommandName, verified.realPath,
		skewLine(executed, verified))
	r.Details = []string{
		fmt.Sprintf("executed (what the fleet runs): %s", executed.describeVerbose()),
		fmt.Sprintf("verified (what probes hit): %s", verified.describeVerbose()),
	}
	r.FixHint = fmt.Sprintf("any capability probe run through PATH %s describes a build the supervisor is not executing; converge the two on one artifact by re-pointing whichever path is stale, then restart the supervisor so it re-executes it — re-pointing a symlink alone does not change a running process's image", gcCommandName)
	return r
}

// reportUnreachableImage covers a running image whose bytes cannot be read at
// all: the file it was loaded from is gone or has been replaced, and the
// platform offers no handle on the executing inode.
//
// This is a finding, not an unverified result. That the supervisor is running
// an artifact with no name on disk is established, actionable, and independent
// of what the bytes turn out to be. It deliberately does NOT claim the two
// binaries differ, because that has not been observed.
//
// state is a parameter rather than a re-derivation because every string below
// asserts one, and only the caller knows which was observed. Three shapes
// reach here, and they need different words and different remediation:
//
//   - the path is still the file the process loaded, and a read of it was
//     refused (a root-owned 0111 artifact, an unreadable parent). Nothing is
//     gone; a restart would re-execute the same file.
//   - the path was replaced or removed under the process — including between
//     classifyImagePath's stat and executedFacts' own, which is the dominant
//     producer and the one a stale state gets wrong.
//   - a platform handle that no longer reaches the executing inode, so the
//     bytes have no name on disk at all.
//
// It must therefore be the state executedFacts OBSERVED. Passing the state
// classifyImagePath returned makes these strings describe a world the caller
// has already disproved.
func (c *BinaryDivergenceCheck) reportUnreachableImage(r *CheckResult, running runningImage, state imagePathState, verified binaryFacts, verifiedPath string) *CheckResult {
	verified.version = c.version(verifiedPath)
	r.Status = StatusWarning
	r.Message = fmt.Sprintf(
		"supervisor pid %d is executing an image whose bytes cannot be read: %s (PATH %s resolves to %s)",
		c.supervisorPID, unreachableReason(running.path, state), gcCommandName, verified.realPath)
	r.Details = []string{
		fmt.Sprintf("executed (what the fleet runs): %s", unreachableDetail(running, state)),
		fmt.Sprintf("verified (what probes hit): %s", verified.describeVerbose()),
	}
	r.FixHint = unreachableFixHint(running, state)
	return r
}

// unreachableFixHint is the remediation for an unreachable image, which
// differs by state as sharply as the diagnosis does. Restarting the supervisor
// converges a fleet whose artifact is gone; for an artifact that is still
// exactly where it was loaded from and merely could not be read, a restart
// changes nothing at all and the operator needs read access instead.
func unreachableFixHint(running runningImage, state imagePathState) string {
	if state == imagePathIntact {
		return fmt.Sprintf("the artifact the supervisor is executing is still at %s, so there is nothing to converge and a restart would re-execute the same file; re-run `%s doctor` as a user that can read it (or under sudo) to compare the bytes",
			running.path, gcCommandName)
	}
	return fmt.Sprintf("no capability probe can describe the running bytes while the artifact behind them is gone; restart the supervisor so it re-executes the artifact now on disk, then re-run `%s doctor` to compare them", gcCommandName)
}

// unreachableReason states why the running bytes could not be read.
//
// state must be the state executedFacts OBSERVED, not the one classifyImagePath
// returned earlier. The two disagree exactly when the artifact moved between
// the two stats, and that is the case whose wording matters most: the intact
// text below asserts three things about the world — that the file is still the
// one the process loaded, that it is still at that path, and that a read of it
// was refused — and all three are false of a path that was replaced under the
// process. Passing the stale state prints them anyway.
func unreachableReason(path string, state imagePathState) string {
	if state == imagePathIntact {
		return fmt.Sprintf("%s is still the file it was loaded from, but its bytes could not be read from there", path)
	}
	return fmt.Sprintf("it is no longer the file at %s — that path was replaced or removed under it, so nothing on disk describes the running bytes", path)
}

// unreachableDetail is unreachableReason's verbose half. It exists because the
// two lines are read together: a Details entry saying the bytes have no name on
// disk, under a Message saying the file is still at its path, asserts a state
// the check did not establish — the same defect one line down.
func unreachableDetail(running runningImage, state imagePathState) string {
	if state == imagePathIntact {
		return fmt.Sprintf("inode %d at %s — the file is still there, but it could not be read, so its bytes were never compared",
			running.id.ino, running.path)
	}
	return fmt.Sprintf("inode %d — reachable only from inside the running process, so whether it matches the binary on PATH cannot be established from here",
		running.id.ino)
}

// unverified reports that the check could not establish whether the two
// binaries agree. It is deliberately distinct from a divergence finding: a
// read that failed is not evidence of a difference, and must not be dressed up
// as one.
func unverified(r *CheckResult, format string, args ...any) *CheckResult {
	return unverifiedWithHint(r,
		"the executed and verified binaries could not be compared, so neither agreement nor divergence is established",
		format, args...)
}

// unverifiedWithHint is unverified with remediation the operator can act on.
func unverifiedWithHint(r *CheckResult, hint, format string, args ...any) *CheckResult {
	r.Status = StatusWarning
	r.Message = "binary divergence unverified: " + fmt.Sprintf(format, args...)
	r.FixHint = hint
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
	// imagePathUnknown means the question could not be answered — a stat that
	// failed for a reason other than the file being gone. Distinct from
	// imagePathReplaced because a permission error is not evidence of a
	// replacement.
	//
	// It is the zero value on purpose. Every tri-state in this file puts the
	// unknown state at iota-zero so that an unset value routes to "we did not
	// establish this" rather than to the permissive answer; a zero that means
	// "intact" would make every accidental one a silent green.
	imagePathUnknown imagePathState = iota
	// imagePathIntact means the path still names the running inode, so the
	// bytes on disk at that path are the bytes the process is running.
	imagePathIntact
	// imagePathReplaced means the artifact was replaced or removed underneath
	// the running process.
	imagePathReplaced
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

// executedFacts returns the on-disk facts describing the running image's
// bytes, whether those bytes are reachable at all, and — the part every
// operator-facing string downstream depends on — the image-path state it
// OBSERVED rather than the one it was handed.
//
// The state it was handed came from classifyImagePath's stat, and is already
// stale: the artifact can be replaced or removed between that stat and this
// one. Reporting from the stale state makes reportUnreachableImage describe a
// world this function has just disproved, which is the same defect as
// asserting a comparison that never ran, in the layer an operator actually
// reads.
//
// An intact image is read at its own path; a replaced one only through a
// platform handle on the executing inode, which not every platform has.
func executedFacts(running runningImage, state imagePathState) (binaryFacts, imagePathState, bool) {
	if state == imagePathIntact {
		f, err := statBinaryErr(running.path)
		if err != nil {
			if os.IsNotExist(err) {
				// Gone since classifyImagePath looked. The path no longer
				// names the running image, whatever it named a moment ago.
				return binaryFacts{}, imagePathReplaced, false
			}
			// Could not look — an unreadable parent directory, an I/O
			// error. The file may well still be there, so the intact
			// observation stands and nothing downstream may claim a
			// replacement.
			return binaryFacts{}, imagePathIntact, false
		}
		// classifyImagePath established this identity at a DIFFERENT stat.
		// Re-asserting it here is what stops a replacement that landed
		// between the two stats from being read as the running image's
		// bytes — the same defect as trusting a /proc handle that no longer
		// reaches the executing inode, reached by a race instead of by a
		// stale name. Bytes that match PATH would otherwise answer "the same
		// binary" for a fleet still running the original.
		id, ok := fileIdentityOf(f.info)
		if !ok {
			// The filesystem stopped exposing an identity it exposed a
			// moment ago. Nothing was disproved, so nothing is claimed.
			return binaryFacts{}, imagePathIntact, false
		}
		if id != running.id {
			return binaryFacts{}, imagePathReplaced, false
		}
		return f, imagePathIntact, true
	}
	if running.contentPath == "" {
		return binaryFacts{}, imagePathReplaced, false
	}
	// Deliberately NOT statBinary: that resolves symlinks first, and resolving
	// /proc/<pid>/exe is precisely the mistake the resolver's own comment
	// warns against — it lands on whatever now sits at the path the link
	// names, which on a replaced image is the file the process is NOT
	// executing. A plain os.Stat of the magic link lands on the executing
	// inode itself. (Today the "(deleted)" marker happens to make the
	// resolution fail, so the bug is one filename away rather than live; the
	// identity assertion below is what makes it not depend on that.)
	f := binaryFacts{path: running.contentPath, realPath: running.contentPath}
	info, err := os.Stat(f.realPath)
	if err != nil {
		return binaryFacts{}, imagePathReplaced, false
	}
	// The handle is only a handle if it still reaches the running inode. A
	// handle that reaches some other file would hand the single verdict-former
	// bytes it never established belong to the process — and those bytes
	// matching PATH would read as a green "same binary" for a fleet running
	// something else.
	if id, ok := fileIdentityOf(info); !ok || id != running.id {
		return binaryFacts{}, imagePathReplaced, false
	}
	f.info = info
	// Report it under the path an operator recognizes, while reading it
	// through the handle that still resolves to the running inode.
	f.display = running.path
	return f, imagePathReplaced, true
}

// version returns the binary's self-declared version, or "" when versionOf is
// unset or the binary does not answer.
func (c *BinaryDivergenceCheck) version(path string) string {
	if c.versionOf == nil {
		return ""
	}
	return c.versionOf(path)
}

// errNoPathLookup reports that this check was built without a PATH lookup at
// all. It is deliberately not exec.ErrNotFound: "we never looked" is not
// evidence that gc is absent from PATH.
var errNoPathLookup = errors.New("no PATH lookup configured")

// resolvePathBinary returns the absolute path PATH resolves gc to.
//
// The three states a lookup can end in are kept apart, because only one of
// them says anything about PATH. A hit is a hit even when it arrives with an
// error attached: exec.LookPath returns exec.ErrDot alongside a usable path
// when the hit came from a relative PATH element, and that binary is exactly
// the one a probe would execute, so it is the one to compare against.
func (c *BinaryDivergenceCheck) resolvePathBinary() (string, error) {
	if c.lookPath == nil {
		return "", errNoPathLookup
	}
	found, err := c.lookPath(gcCommandName)
	if found == "" {
		if err == nil {
			return "", fmt.Errorf("PATH lookup for %s returned neither a path nor an error", gcCommandName)
		}
		return "", err
	}
	abs, absErr := filepath.Abs(found)
	if absErr != nil {
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
	// It is also where the bytes are read from.
	realPath string
	// display overrides realPath in output. Set when the bytes are reached
	// through a handle (/proc/<pid>/exe) whose path would mean nothing to an
	// operator.
	display string
	// info is the stat of realPath; nil when it could not be stat'd.
	info os.FileInfo
	// version is the binary's self-declared version. Populated only on the
	// divergent path, the one place it is worth running a binary to learn.
	version string
}

// readPath is where these bytes are actually read from.
func (f binaryFacts) readPath() string { return f.realPath }

// statBinary resolves and stats one binary, tolerating failures so the check
// still reports the paths it does know.
func statBinary(path string) binaryFacts {
	f, _ := statBinaryErr(path)
	return f
}

// statBinaryErr is statBinary with the stat error kept. A caller that must
// tell "the file is gone" from "the file could not be looked at" needs the
// error itself: a nil info collapses those two into one, and they license
// opposite claims about the state of the world.
func statBinaryErr(path string) (binaryFacts, error) {
	f := binaryFacts{path: path, realPath: path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		f.realPath = resolved
	}
	info, err := os.Stat(f.realPath)
	if err != nil {
		return f, err
	}
	f.info = info
	return f, nil
}

// describePath is the single path to name in a one-line message.
func (f binaryFacts) describePath() string {
	if f.display != "" {
		return f.display
	}
	return f.realPath
}

// describe renders the short form used when the two sides agree.
func (f binaryFacts) describe() string {
	if f.display != "" {
		return f.display
	}
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
		return fmt.Sprintf("the verified binary is NEWER %s: probes are verifying ahead of the running fleet", describeSkew(vt.Sub(rt)))
	case rt.After(vt):
		return fmt.Sprintf("the executed binary is NEWER %s: probes are verifying behind the running fleet", describeSkew(rt.Sub(vt)))
	default:
		return "both binaries share a modification time but differ in content"
	}
}

// describeSkew renders a positive duration without rounding it away. Two
// binaries written a few hundred milliseconds apart are still one newer than
// the other, and "NEWER by 0s" reads as a contradiction.
func describeSkew(d time.Duration) string {
	if d < time.Second {
		return "by under a second"
	}
	return "by " + d.Round(time.Second).String()
}

// contentOutcome is the result of comparing two files' bytes. It is a
// tri-state for the same reason every other step here is: a file that could
// not be read has not been shown to differ.
type contentOutcome int

const (
	// contentUnknown means at least one file could not be read. It is the
	// zero value for the same reason imagePathUnknown is: an unset outcome
	// must route to "not established", never to a verdict.
	contentUnknown contentOutcome = iota
	// contentSame means both files were read and hold identical bytes.
	contentSame
	// contentDiffers means both files were read and hold different bytes, or
	// their sizes already settled it.
	contentDiffers
)

// compareFailure renders why a content comparison produced no usable answer.
// An outcome with no verdict attached is a defect in this file rather than a
// condition on the host, and it says so rather than borrowing the wording of a
// failed read.
func compareFailure(outcome contentOutcome, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("unclassified content comparison outcome %d", int(outcome))
}

// contentChunkSize bounds both the tail sample and the streaming read buffer.
const contentChunkSize = 64 << 10

// compareContent reports whether two distinct files hold identical bytes.
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
func compareContent(a, b binaryFacts) (contentOutcome, string, error) {
	if a.info == nil || b.info == nil {
		return contentUnknown, "", fmt.Errorf("at least one binary could not be stat'd")
	}
	if a.info.Size() != b.info.Size() {
		return contentDiffers, "", nil
	}
	switch same, err := sameTail(a.readPath(), b.readPath(), a.info.Size()); {
	case err != nil:
		return contentUnknown, "", err
	case !same:
		return contentDiffers, "", nil
	}
	sum, err := compareAndDigest(a.readPath(), b.readPath())
	if err != nil {
		return contentUnknown, "", err
	}
	if sum == "" {
		return contentDiffers, "", nil
	}
	return contentSame, fmt.Sprintf("%s and %s are different files with identical content (sha256 %s)",
		a.describePath(), b.describePath(), sum[:12]), nil
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
// That same property makes the link a readable handle on the running bytes,
// which is what lets a replaced image still be compared rather than merely
// reported as gone.
//
// procRoot is a parameter so the read is testable on hosts without procfs.
func runningImageFromProc(procRoot string, pid int) (runningImage, error) {
	link := filepath.Join(procRoot, strconv.Itoa(pid), "exe")
	target, err := os.Readlink(link)
	if err != nil {
		return runningImage{}, fmt.Errorf("reading %s: %w", link, err)
	}
	img := runningImage{contentPath: link}
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
//
// No contentPath is set: macOS has no handle on an executing inode once its
// path stops naming it, so a replaced image's bytes are genuinely unreachable
// and the check reports that rather than guessing at them.
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
//
// The set holds the dynamic loader, every mapped library, and — for a binary
// that links ICU, as gc does — mapped data files such as locale tables and
// icudt*.dat. Two passes, because the obvious one has a blind spot:
//
//  1. The entry that is itself an executable. This asks the file format rather
//     than denylisting suffixes, so a data file ordered ahead of the image
//     cannot be mistaken for it.
//
//  2. Failing that, the SOLE entry pass 1 could not evaluate at all: one whose
//     file is no longer the file lsof recorded (gone, or a different inode than
//     the one mapped), or one that cannot be opened for reading. Pass 1 answers
//     its question by reading the file at the reported path, so it is blind to
//     exactly the two states most worth reporting — an image deleted or
//     overwritten underneath the process, and an image the operator lacks
//     permission to read. A process executes exactly one image, and pass 1
//     established that none of the entries it could evaluate is an executable,
//     so when exactly one entry is unevaluatable that entry is the image.
//
// "Sole" is load-bearing. When several entries are unevaluatable — an
// installer that replaced the whole tree, libraries and executable together —
// nothing distinguishes the image from the libraries beside it, and picking
// the first is a guess dressed as an answer. Pass 2 refuses instead.
//
// Pass 2's inference is sound only under two assumptions, neither of which
// this function can check, and both of which can still put a library in its
// answer:
//
//   - lsof reported the COMPLETE mapped-text set. runningImageFromLsof accepts
//     partial output from a probe killed at its deadline (ga-s5cyd), and a
//     truncated set can omit the executable entirely — leaving a library as
//     the sole unevaluatable entry and this pass confident about it.
//   - machOIsExecutable has no false negatives on a readable, present image.
//     Pass 1 concluding "no executable here" is read below as "the image is
//     one of the ones I could not evaluate", which is false if the image was
//     readable and merely unrecognized — a shell wrapper installed over the
//     path, or a format the predicate does not know.
//
// Both were true of the reproduced defect this shape replaced, where lsof
// ordering alone decided the answer. Neither is guaranteed.
//
// When neither pass finds anything the answer is that it could not be
// determined. Naming a shared library as the executed image — with a restart
// hint attached — would be worse than saying so.
func selectRunningImage(entries []lsofEntry, isExecutable func(string) bool) (runningImage, error) {
	if len(entries) == 0 {
		return runningImage{}, fmt.Errorf("lsof reported no mapped-text entries")
	}
	for _, e := range entries {
		path, _ := strings.CutSuffix(e.name, deletedImageSuffix)
		if path != "" && isExecutable(path) {
			return imageFromLsofEntry(e)
		}
	}
	var candidate runningImage
	unevaluatable := 0
	for _, e := range entries {
		img, err := imageFromLsofEntry(e)
		if err != nil {
			continue
		}
		if classifyImagePath(img) == imagePathReplaced || !fileIsReadable(img.path) {
			candidate = img
			unevaluatable++
		}
	}
	switch unevaluatable {
	case 1:
		return candidate, nil
	case 0:
		return runningImage{}, fmt.Errorf(
			"no executable image among %d mapped-text entries, and every one of them is still the readable file lsof recorded", len(entries))
	default:
		return runningImage{}, fmt.Errorf(
			"no executable image among %d mapped-text entries, and %d of them are no longer the readable files lsof recorded — which one is the executed image cannot be told from the libraries beside it",
			len(entries), unevaluatable)
	}
}

// fileIsReadable reports whether a path can be opened for reading. It is the
// difference between "this is not an executable" and "this could not be
// examined", which is the distinction pass 2 of selectRunningImage turns on.
func fileIsReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	f.Close() //nolint:errcheck // read-only handle, opened only to test access
	return true
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

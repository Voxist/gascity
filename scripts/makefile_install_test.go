//go:build integration

// Behavioral contracts for the two Makefile targets that place a gc binary.
//
// `install` writes exactly one path, $(INSTALL_DIR)/$(BINARY). `deploy-fleet`
// is the separate, explicit step that moves a running deployment onto a build
// (vc-lwif: both used to write ~/.local/bin/gc, last writer winning in
// silence). The static text contracts live in makefile_install_ownership_test.go.
//
// Every subprocess in this file goes through runTool, which owns the single
// exec.Command call site: the checked resource census in
// internal/testpolicy/resourcecensus counts call sites, and these tests must
// not grow that ratchet to prove a Makefile target works.

package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runTool runs one command and returns its combined output. It is the only
// place in this file that spawns a process.
func runTool(t *testing.T, dir string, env []string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// installTestMakefile writes a copy of the repo Makefile with `install`'s
// check-self-contained prerequisite dropped, so the target can be exercised
// without a real multi-minute gc build. Returns its path.
func installTestMakefile(t *testing.T, repoRoot, dir string) string {
	t.Helper()
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	text := string(makefile)
	if !strings.Contains(text, "\ninstall: check-self-contained\n") {
		t.Fatal("Makefile install target no longer depends on check-self-contained as expected")
	}
	path := filepath.Join(dir, "Makefile")
	body := strings.Replace(text, "\ninstall: check-self-contained\n", "\ninstall:\n", 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write test Makefile: %v", err)
	}
	return path
}

func TestMakeInstallFailsClosedWhenCopyFails(t *testing.T) {
	repo := repoRoot(t)
	tmp := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(tmp, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(path, 0o755)
			} else {
				_ = os.Chmod(path, 0o644)
			}
			return nil
		})
	})
	buildDir := filepath.Join(tmp, "build")
	installDir := filepath.Join(tmp, "install")
	binDir := filepath.Join(tmp, "bin")
	for _, dir := range []string{buildDir, installDir, binDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	sourceBinary := filepath.Join(buildDir, "gc")
	if err := os.WriteFile(sourceBinary, []byte("new binary"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	installedBinary := filepath.Join(installDir, "gc")
	if err := os.WriteFile(installedBinary, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("write installed binary: %v", err)
	}

	writeExecutable(t, filepath.Join(binDir, "cp"), `#!/usr/bin/env sh
for last do :; done
printf 'partial binary' > "$last"
exit 1
`)

	out, err := runTool(t, repo, append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+filepath.Join(tmp, "home"),
	), "make", "--no-print-directory", "-f", installTestMakefile(t, repo, tmp), "install",
		"BUILD_DIR="+buildDir,
		"INSTALL_DIR="+installDir,
		"BINARY=gc",
	)
	if err == nil {
		t.Fatalf("make install succeeded after cp failure:\n%s", out)
	}

	content, readErr := os.ReadFile(installedBinary)
	if readErr != nil {
		t.Fatalf("read installed binary: %v\nmake output:\n%s", readErr, out)
	}
	if string(content) != "old binary" {
		t.Fatalf("installed binary = %q, want old binary after cp failure\nmake output:\n%s", content, out)
	}

	entries, readDirErr := os.ReadDir(installDir)
	if readDirErr != nil {
		t.Fatalf("read install dir: %v", readDirErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".gc.tmp.") {
			t.Fatalf("temporary install file was not cleaned up: %s\nmake output:\n%s", entry.Name(), out)
		}
	}
}

// installFixture lays out a fake HOME with a .local/bin channel plus separate
// build and install directories, and returns them.
// homeDerivedChannels are the GC_DEPLOY_CHANNELS entries that follow $(HOME),
// so a sandboxed HOME redirects them. $(BIN_DIR) does not follow HOME and is
// overridden separately via INSTALL_DIR.
var homeDerivedChannels = []string{
	filepath.Join(".local", "bin"),
	filepath.Join(".gc", "bin"),
}

func installFixture(t *testing.T) (home, buildDir, installDir, localBin string) {
	t.Helper()
	tmp := t.TempDir()
	home = filepath.Join(tmp, "home")
	buildDir = filepath.Join(tmp, "build")
	installDir = filepath.Join(tmp, "install")
	localBin = filepath.Join(home, ".local", "bin")
	dirs := []string{buildDir, installDir}
	for _, rel := range homeDerivedChannels {
		dirs = append(dirs, filepath.Join(home, rel))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(buildDir, "gc"), []byte("dev build"), 0o755); err != nil {
		t.Fatalf("write source binary: %v", err)
	}
	return home, buildDir, installDir, localBin
}

func runInstall(t *testing.T, home, buildDir, installDir string, extra ...string) (string, error) {
	t.Helper()
	repo := repoRoot(t)
	// GC_DEPLOY_CHANNELS is deliberately left at its default: under a
	// sandboxed HOME it expands to the sandbox's own channels, so a
	// reintroduced write by ANY spelling fails these tests rather than being
	// masked by an explicit override. $(BIN_DIR) is the one entry that does
	// not follow HOME, and INSTALL_DIR redirects the only write there is.
	args := []string{
		"--no-print-directory", "-f",
		installTestMakefile(t, repo, filepath.Dir(home)), "install",
		"BUILD_DIR=" + buildDir,
		"INSTALL_DIR=" + installDir,
		"BINARY=gc",
	}
	args = append(args, extra...)
	return runTool(t, repo, append(os.Environ(), "HOME="+home), "make", args...)
}

// TestMakeInstallLeavesDeployChannelsUntouched is the behavioral half of the
// vc-lwif invariant: installing writes $(INSTALL_DIR)/$(BINARY) and nothing
// else. A deployment's ~/.local/bin/gc must survive an install byte for byte,
// still a real binary rather than a symlink at the dev build.
func TestMakeInstallLeavesDeployChannelsUntouched(t *testing.T) {
	home, buildDir, installDir, localBin := installFixture(t)
	deployed := filepath.Join(localBin, "gc")
	if err := os.WriteFile(deployed, []byte("release binary"), 0o755); err != nil {
		t.Fatalf("write deployed binary: %v", err)
	}
	gcBinChannel := filepath.Join(home, ".gc", "bin", "gc")

	out, err := runInstall(t, home, buildDir, installDir)
	if err != nil {
		t.Fatalf("make install: %v\n%s", err, out)
	}

	info, err := os.Lstat(deployed)
	if err != nil {
		t.Fatalf("lstat deployed binary: %v\nmake output:\n%s", err, out)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, _ := os.Readlink(deployed)
		t.Fatalf("make install replaced the deployed binary with a symlink -> %s\nmake output:\n%s", link, out)
	}
	content, err := os.ReadFile(deployed)
	if err != nil {
		t.Fatalf("read deployed binary: %v", err)
	}
	if string(content) != "release binary" {
		t.Fatalf("deployed binary = %q, want it untouched by make install\nmake output:\n%s", content, out)
	}
	if got, readErr := os.ReadFile(filepath.Join(installDir, "gc")); readErr != nil || string(got) != "dev build" {
		t.Fatalf("install dir binary = %q (err %v), want the dev build", got, readErr)
	}
	// $(GC_DEPLOY_DIR)/gc had no integration coverage at all, which is where a
	// variable-driven write would have slipped both tiers.
	if _, statErr := os.Lstat(gcBinChannel); !os.IsNotExist(statErr) {
		t.Fatalf("make install created %s; it must touch no channel but the install path (%v)",
			gcBinChannel, statErr)
	}
}

// TestMakeInstallWarnsWhenADeployChannelPointsAtTheInstallPath covers the
// residual hazard the symlink removal alone does not close: when a channel is
// already a symlink AT $(INSTALL_DIR)/$(BINARY), overwriting that path moves
// the deployment whether or not install relinks anything. Installing may not
// do that silently.
func TestMakeInstallWarnsWhenADeployChannelPointsAtTheInstallPath(t *testing.T) {
	home, buildDir, installDir, localBin := installFixture(t)
	if err := os.WriteFile(filepath.Join(installDir, "gc"), []byte("old"), 0o755); err != nil {
		t.Fatalf("seed install dir: %v", err)
	}
	deployed := filepath.Join(localBin, "gc")
	if err := os.Symlink(filepath.Join(installDir, "gc"), deployed); err != nil {
		t.Fatalf("symlink deployed binary: %v", err)
	}

	out, err := runInstall(t, home, buildDir, installDir)
	if err != nil {
		t.Fatalf("make install: %v\n%s", err, out)
	}
	if !strings.Contains(out, deployed) {
		t.Fatalf("make install did not name the deploy channel it moved (%s):\n%s", deployed, out)
	}
	if !strings.Contains(out, "WARNING") {
		t.Fatalf("make install must warn when it moves a deployment:\n%s", out)
	}
}

// TestMakeInstallRefusesToOverwriteADeployedChannelAtTheInstallPath is the
// state this host is actually in: $(INSTALL_DIR)/$(BINARY) is itself a deploy
// channel, and on a deployed machine it is a symlink at an artifact elsewhere.
// Overwriting it moves every context that resolves gc through it. A warning
// cannot help -- it can only print once the channel has already moved -- so
// install fails closed, the same standard deploy-fleet is held to.
func TestMakeInstallRefusesToOverwriteADeployedChannelAtTheInstallPath(t *testing.T) {
	home, buildDir, installDir, _ := installFixture(t)
	artifact := filepath.Join(home, ".gc", "bin", "gc-main-20260904-deadbeef")
	writeExecutable(t, artifact, "#!/usr/bin/env sh\nexit 0\n")
	channel := filepath.Join(installDir, "gc")
	if err := os.Symlink(artifact, channel); err != nil {
		t.Fatalf("symlink deployed channel: %v", err)
	}

	out, err := runInstall(t, home, buildDir, installDir)
	if err == nil {
		t.Fatalf("make install overwrote a deployed channel and exited 0:\n%s", out)
	}
	if !strings.Contains(out, artifact) {
		t.Errorf("the refusal must name the artifact the channel points at (%s):\n%s", artifact, out)
	}
	if !strings.Contains(out, "GC_ALLOW_CHANNEL_OVERWRITE=1") {
		t.Errorf("the refusal must name the escape hatch:\n%s", out)
	}
	link, readErr := os.Readlink(channel)
	if readErr != nil || link != artifact {
		t.Fatalf("channel = %q (err %v), want it left as a symlink at %s\n%s",
			link, readErr, artifact, out)
	}
}

// The escape hatch must actually work, or the refusal is a wall rather than a
// gate.
func TestMakeInstallOverwritesADeployedChannelWhenExplicitlyAllowed(t *testing.T) {
	home, buildDir, installDir, _ := installFixture(t)
	artifact := filepath.Join(home, ".gc", "bin", "gc-main-20260904-deadbeef")
	writeExecutable(t, artifact, "#!/usr/bin/env sh\nexit 0\n")
	channel := filepath.Join(installDir, "gc")
	if err := os.Symlink(artifact, channel); err != nil {
		t.Fatalf("symlink deployed channel: %v", err)
	}

	out, err := runInstall(t, home, buildDir, installDir, "GC_ALLOW_CHANNEL_OVERWRITE=1")
	if err != nil {
		t.Fatalf("make install refused despite GC_ALLOW_CHANNEL_OVERWRITE=1: %v\n%s", err, out)
	}
	got, readErr := os.ReadFile(channel)
	if readErr != nil || string(got) != "dev build" {
		t.Fatalf("channel = %q (err %v), want the dev build\n%s", got, readErr, out)
	}
}

// A symlink INSIDE the install directory is a local convenience, not a
// deployment, and must not trip the gate.
func TestMakeInstallAllowsALocalSymlinkInsideTheInstallDir(t *testing.T) {
	home, buildDir, installDir, _ := installFixture(t)
	sibling := filepath.Join(installDir, "gc.previous")
	if err := os.WriteFile(sibling, []byte("previous"), 0o755); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	if err := os.Symlink(sibling, filepath.Join(installDir, "gc")); err != nil {
		t.Fatalf("symlink local: %v", err)
	}

	out, err := runInstall(t, home, buildDir, installDir)
	if err != nil {
		t.Fatalf("make install refused a purely local symlink: %v\n%s", err, out)
	}
	if got, readErr := os.ReadFile(filepath.Join(installDir, "gc")); readErr != nil || string(got) != "dev build" {
		t.Fatalf("install path = %q (err %v), want the dev build\n%s", got, readErr, out)
	}
}

// TestMakeInstallDoesNotClaimTheChannelsWereUntouched: $(INSTALL_DIR)/$(BINARY)
// is a deploy channel and install writes it every run, so a blanket "channels
// were NOT touched" is false by the Makefile's own definitions. Given the bead
// is "the machine ran a stale image with nothing saying so", shipping a
// definitionally wrong reassurance is its own defect.
func TestMakeInstallDoesNotClaimTheChannelsWereUntouched(t *testing.T) {
	home, buildDir, installDir, _ := installFixture(t)

	out, err := runInstall(t, home, buildDir, installDir)
	if err != nil {
		t.Fatalf("make install: %v\n%s", err, out)
	}
	if strings.Contains(out, "Deploy channels were NOT touched") {
		t.Errorf("install claims no channel was touched, but it wrote %s, which is one:\n%s",
			filepath.Join(installDir, "gc"), out)
	}
	if !strings.Contains(out, filepath.Join(installDir, "gc")) {
		t.Errorf("install must name the channel it overwrote:\n%s", out)
	}
}

// deployFleetFixture lays out the three PATH channels and a runnable stand-in
// binary, and returns the binary path and the channels.
func deployFleetFixture(t *testing.T) (root, binary string, channels []string) {
	t.Helper()
	root = t.TempDir()
	binDir := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", binDir, err)
	}
	binary = filepath.Join(binDir, "gc-main-20260905-deadbeef")
	writeExecutable(t, binary, "#!/usr/bin/env sh\nexit 0\n")

	for _, rel := range []string{".local/bin", "go/bin", ".gc/bin"} {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		channels = append(channels, filepath.Join(dir, "gc"))
	}
	return root, binary, channels
}

func runDeployFleet(t *testing.T, binary string, channels []string) (string, error) {
	t.Helper()
	return runTool(t, repoRoot(t), nil, "make", "--no-print-directory", "deploy-fleet",
		"GC_DEPLOY_BINARY="+binary,
		"GC_DEPLOY_CHANNELS="+strings.Join(channels, " "),
		"BINARY=gc",
	)
}

func TestMakeDeployFleetRepointsEveryChannel(t *testing.T) {
	_, binary, channels := deployFleetFixture(t)
	// One channel already holds a stale real binary, another a stale link.
	if err := os.WriteFile(channels[0], []byte("stale"), 0o755); err != nil {
		t.Fatalf("seed stale binary: %v", err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), channels[1]); err != nil {
		t.Fatalf("seed dangling link: %v", err)
	}

	out, err := runDeployFleet(t, binary, channels)
	if err != nil {
		t.Fatalf("make deploy-fleet: %v\n%s", err, out)
	}
	for _, channel := range channels {
		info, lstatErr := os.Lstat(channel)
		if lstatErr != nil {
			t.Fatalf("lstat %s: %v\n%s", channel, lstatErr, out)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is not a symlink -- deploy-fleet must link, never copy "+
				"(cp strips the macOS codesign)\n%s", channel, out)
			continue
		}
		link, readErr := os.Readlink(channel)
		if readErr != nil {
			t.Fatalf("readlink %s: %v", channel, readErr)
		}
		if link != binary {
			t.Errorf("%s -> %s, want %s\n%s", channel, link, binary, out)
		}
		if !strings.Contains(out, channel) {
			t.Errorf("deploy-fleet did not report repointing %s:\n%s", channel, out)
		}
	}
}

// TestMakeDeployFleetResolvesSymlinkChains pins the legibility property: a
// channel must name the real, provenance-named file, so `ls -l` shows which
// build is deployed rather than another hop.
func TestMakeDeployFleetResolvesSymlinkChains(t *testing.T) {
	root, binary, channels := deployFleetFixture(t)
	hop := filepath.Join(root, "artifacts", "gc")
	if err := os.Symlink(binary, hop); err != nil {
		t.Fatalf("symlink hop: %v", err)
	}

	out, err := runDeployFleet(t, hop, channels)
	if err != nil {
		t.Fatalf("make deploy-fleet: %v\n%s", err, out)
	}
	for _, channel := range channels {
		link, readErr := os.Readlink(channel)
		if readErr != nil {
			t.Fatalf("readlink %s: %v\n%s", channel, readErr, out)
		}
		if link != binary {
			t.Errorf("%s -> %s, want the resolved artifact %s\n%s", channel, link, binary, out)
		}
	}
}

// TestMakeDeployFleetRefusesABinaryThatDoesNotRun is the guard that makes the
// no-copy rule enforceable rather than aspirational: whatever damages a build
// -- a broken codesign giving SIGKILL 137, a truncated write, the wrong
// architecture -- proving the binary runs must happen before any channel
// moves, because a channel that has moved is a deployment that has moved.
func TestMakeDeployFleetRefusesABinaryThatDoesNotRun(t *testing.T) {
	_, binary, channels := deployFleetFixture(t)
	writeExecutable(t, binary, "#!/usr/bin/env sh\nexit 137\n")
	for _, channel := range channels {
		if err := os.WriteFile(channel, []byte("good"), 0o755); err != nil {
			t.Fatalf("seed channel: %v", err)
		}
	}

	out, err := runDeployFleet(t, binary, channels)
	if err == nil {
		t.Fatalf("make deploy-fleet succeeded with a binary that does not run:\n%s", out)
	}
	for _, channel := range channels {
		content, readErr := os.ReadFile(channel)
		if readErr != nil || string(content) != "good" {
			t.Errorf("%s = %q (err %v), want it untouched after the refusal\n%s",
				channel, content, readErr, out)
		}
	}
}

func TestMakeDeployFleetRefusesAMissingBinary(t *testing.T) {
	root, _, channels := deployFleetFixture(t)
	out, err := runDeployFleet(t, filepath.Join(root, "absent"), channels)
	if err == nil {
		t.Fatalf("make deploy-fleet succeeded with a missing binary:\n%s", out)
	}
}

func TestMakeDeployFleetSkipsChannelsWhoseDirectoryIsAbsent(t *testing.T) {
	root, binary, channels := deployFleetFixture(t)
	absent := filepath.Join(root, "not-on-this-machine", "bin", "gc")

	out, err := runDeployFleet(t, binary, append(channels, absent))
	if err != nil {
		t.Fatalf("make deploy-fleet: %v\n%s", err, out)
	}
	if _, statErr := os.Lstat(absent); !os.IsNotExist(statErr) {
		t.Fatalf("deploy-fleet created %s in a directory that does not exist (%v)", absent, statErr)
	}
	if link, _ := os.Readlink(channels[0]); link != binary {
		t.Fatalf("%s -> %q, want %s\n%s", channels[0], link, binary, out)
	}
}

// TestMakeDeployFleetNeverLinksTheBuildOverItself guards the DEFAULT
// invocation: GC_DEPLOY_BINARY defaults to $(INSTALL_DIR)/$(BINARY), which is
// itself one of the channels. `ln -sfn x x` unlinks the real binary and leaves
// a self-referential dangling link in its place, so `make install &&
// make deploy-fleet` would delete the build it just made.
func TestMakeDeployFleetNeverLinksTheBuildOverItself(t *testing.T) {
	_, binary, channels := deployFleetFixture(t)
	channels = append(channels, binary)

	out, err := runDeployFleet(t, binary, channels)
	if err != nil {
		t.Fatalf("make deploy-fleet: %v\n%s", err, out)
	}
	info, err := os.Lstat(binary)
	if err != nil {
		t.Fatalf("the build itself is gone after deploy-fleet: %v\n%s", err, out)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, _ := os.Readlink(binary)
		t.Fatalf("deploy-fleet replaced the build with a symlink -> %s\n%s", link, out)
	}
	for _, channel := range channels[:len(channels)-1] {
		if link, _ := os.Readlink(channel); link != binary {
			t.Errorf("%s -> %q, want %s\n%s", channel, link, binary, out)
		}
	}
}

// The self-link guard must compare FILE IDENTITY, not path text. `ln -sfn x x`
// unlinks the real binary and leaves a self-referential loop in its place, and
// the readlink verification afterwards still PASSES -- the link holds exactly
// what it was asked to hold -- so deploy-fleet would exit 0 and report success
// over a destroyed, unrecoverable build. Every same-file/different-spelling
// pairing below is a way to reach that, and a guard that only recognises the
// default spelling lets each one through.

// TestMakeDeployFleetKeepsABuildNamedByANonNormalizedPath covers a
// GC_DEPLOY_BINARY that names a channel by a different spelling of the same
// path (here an interior "/./"). Chain resolution does not normalize it,
// because the final component is a regular file and the loop never runs.
func TestMakeDeployFleetKeepsABuildNamedByANonNormalizedPath(t *testing.T) {
	_, _, channels := deployFleetFixture(t)
	build := channels[1] // <root>/go/bin/gc
	writeExecutable(t, build, "#!/usr/bin/env sh\nexit 0\n")
	spelling := filepath.Dir(build) + "/./" + filepath.Base(build)

	out, err := runDeployFleet(t, spelling, channels)
	if err != nil {
		t.Fatalf("make deploy-fleet: %v\n%s", err, out)
	}
	assertBuildSurvived(t, build, out)
}

// TestMakeDeployFleetKeepsABuildReachedThroughASymlinkedChannelDirectory
// covers a channel whose DIRECTORY is a symlink (~/.local/bin -> ~/go/bin), so
// two channel paths are the same file while sharing no path text. The guard
// firing correctly for the second channel does not help: the first has already
// been linked over itself.
func TestMakeDeployFleetKeepsABuildReachedThroughASymlinkedChannelDirectory(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "go", "bin")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", realDir, err)
	}
	linkedDir := filepath.Join(root, "localbin")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Fatalf("symlink channel dir: %v", err)
	}
	build := filepath.Join(realDir, "gc")
	writeExecutable(t, build, "#!/usr/bin/env sh\nexit 0\n")

	// The symlinked-directory channel is listed FIRST, as it is on a host
	// whose PATH reaches the same binary by both spellings.
	out, err := runDeployFleet(t, build, []string{filepath.Join(linkedDir, "gc"), build})
	if err != nil {
		t.Fatalf("make deploy-fleet: %v\n%s", err, out)
	}
	assertBuildSurvived(t, build, out)
}

// TestMakeDeployFleetKeepsABuildReachedThroughAHardLink covers the third
// distinct filesystem mechanism for one file under two names: same inode, two
// directory entries, no symlink anywhere. Unlinking such a channel drops a
// link, and if it is the last one the build is gone.
func TestMakeDeployFleetKeepsABuildReachedThroughAHardLink(t *testing.T) {
	_, _, channels := deployFleetFixture(t)
	build := channels[1] // <root>/go/bin/gc
	writeExecutable(t, build, "#!/usr/bin/env sh\nexit 0\n")
	hard := channels[0] // <root>/.local/bin/gc
	if err := os.Link(build, hard); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}

	out, err := runDeployFleet(t, build, channels)
	if err != nil {
		t.Fatalf("make deploy-fleet: %v\n%s", err, out)
	}
	assertBuildSurvived(t, build, out)
	assertBuildSurvived(t, hard, out)
}

// assertBuildSurvived fails if the binary was replaced by a link or is no
// longer executable. A self-referential link satisfies `readlink`, so checking
// the link text proves nothing -- run the file.
func assertBuildSurvived(t *testing.T, build, out string) {
	t.Helper()
	info, err := os.Lstat(build)
	if err != nil {
		t.Fatalf("the build is gone after deploy-fleet: %v\n%s", err, out)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, _ := os.Readlink(build)
		t.Fatalf("deploy-fleet linked the build over itself: %s -> %s\n%s", build, link, out)
	}
	if runOut, runErr := runTool(t, "", nil, build, "version"); runErr != nil {
		t.Fatalf("the build no longer runs after deploy-fleet: %v\n%s\ndeploy output:\n%s",
			runErr, runOut, out)
	}
}

// TestMakeDeployFleetPreservesTheImmutableFlag: bd is pinned uchg on this
// fleet, so a gc channel can be too. A write that silently fails against an
// immutable file is the failure this guards.
func TestMakeDeployFleetPreservesTheImmutableFlag(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("uchg is a BSD/macOS file flag")
	}
	_, binary, channels := deployFleetFixture(t)
	pinned := channels[0]
	if err := os.WriteFile(pinned, []byte("pinned"), 0o755); err != nil {
		t.Fatalf("seed pinned channel: %v", err)
	}
	if out, err := runTool(t, "", nil, "chflags", "uchg", pinned); err != nil {
		t.Skipf("chflags uchg unavailable: %v\n%s", err, out)
	}
	// -h: after the deploy the channel is a symlink, and chflags without it
	// would clear the flag on the artifact instead, leaving the link immutable
	// and TempDir cleanup unable to remove it.
	t.Cleanup(func() { _, _ = runTool(t, "", nil, "chflags", "-h", "nouchg", pinned) })

	out, err := runDeployFleet(t, binary, channels)
	if err != nil {
		t.Fatalf("make deploy-fleet: %v\n%s", err, out)
	}
	link, readErr := os.Readlink(pinned)
	if readErr != nil {
		t.Fatalf("readlink %s: %v\n%s", pinned, readErr, out)
	}
	if link != binary {
		t.Fatalf("%s -> %s, want %s\n%s", pinned, link, binary, out)
	}
	flags, lsErr := runTool(t, "", nil, "ls", "-ldO", pinned)
	if lsErr != nil {
		t.Fatalf("ls -ldO %s: %v\n%s", pinned, lsErr, flags)
	}
	if !strings.Contains(flags, "uchg") {
		t.Fatalf("deploy-fleet dropped the uchg flag on %s: %s", pinned, flags)
	}
}

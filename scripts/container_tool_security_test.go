package scripts_test

import (
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestContainerCLIToolsRebuildWithPatchedGRPC(t *testing.T) {
	const (
		ghVersion     = "2.96.0"
		ghSourceRef   = "b300f2ec7ec9dc9addc39b2ad88c54097ded7ca0"
		doltSourceRef = "781cbb730221ea7df4fc7995255bb336df9c3864"
		grpcVersion   = "1.83.2"
		xtextVersion  = "0.41.0"
		// Floors for the gh and dolt builds; see Dockerfile.base.
		xcryptoVersion            = "0.56.0"
		xmodVersion               = "0.40.0"
		thriftVersion             = "0.24.0"
		ghSourceSHA256            = "a0c18c98c73f7333f73e19b3a0bf5bd18673f3dc226193ab6478b3ea1ea18f03"
		doltSourceSHA256          = "0b0c9bce8baef26baa7e0e5825cd2d7d6101daf6fc9673f38dac9670afb66847"
		doltToolchainRelease      = "20260611_0.0.5_trixie"
		doltOptcrossX8664SHA256   = "caf703fb1cbc0c9ff9a5b506f73da6c6f5233c04a455e638cdc50267a4d0c0c0"
		doltOptcrossAarch64SHA256 = "5635d0b38343fefb0c2b600d61c49ad9ceeaa1107bccdec8a60b1789100dc0ce"
		doltICUStaticSHA256       = "8b0234f16da73b9c8d47f86eeef98928879611149e3ee1bb560dddb0ffdd95a1"
	)

	dockerfile := readFile(t, repoRoot(t), "contrib/k8s/Dockerfile.base")
	for _, want := range []string{
		"ARG GH_VERSION=" + ghVersion,
		"ARG GH_SOURCE_REF=" + ghSourceRef,
		"ARG GH_SOURCE_SHA256=" + ghSourceSHA256,
		"ARG DOLT_SOURCE_REF=" + doltSourceRef,
		"ARG DOLT_SOURCE_SHA256=" + doltSourceSHA256,
		"ARG GRPC_VERSION=" + grpcVersion,
		"ARG XTEXT_VERSION=" + xtextVersion,
		"ARG XCRYPTO_VERSION=" + xcryptoVersion,
		"ARG XMOD_VERSION=" + xmodVersion,
		"ARG THRIFT_VERSION=" + thriftVersion,
		"ARG DOLT_TOOLCHAIN_RELEASE=" + doltToolchainRelease,
		"ARG DOLT_OPTCROSS_X86_64_SHA256=" + doltOptcrossX8664SHA256,
		"ARG DOLT_OPTCROSS_AARCH64_SHA256=" + doltOptcrossAarch64SHA256,
		"ARG DOLT_ICU_STATIC_SHA256=" + doltICUStaticSHA256,
		`grep -Fq "Version = \"${DOLT_VERSION}\"" cmd/dolt/doltversion/version.go`,
		`CGO_LDFLAGS="-static -s"`,
		`-tags="icu_static,timetzdata"`,
		"x86_64-linux-musl-gcc",
		"aarch64-linux-musl-gcc",
		`file /out/dolt | grep -Fq "statically linked"`,
		"COPY --from=tool-builder /out/gh /usr/bin/gh",
		"COPY --from=tool-builder /out/dolt /usr/local/bin/dolt",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("contrib/k8s/Dockerfile.base missing %q", want)
		}
	}
	if got := strings.Count(dockerfile, `go get "google.golang.org/grpc@v${GRPC_VERSION}"`); got != 2 {
		t.Errorf("contrib/k8s/Dockerfile.base applies the grpc override %d times, want exactly 2 (gh and Dolt)", got)
	}
	for _, override := range []string{
		`go get "golang.org/x/crypto@v${XCRYPTO_VERSION}"`,
		`go get "golang.org/x/mod@v${XMOD_VERSION}"`,
	} {
		if got := strings.Count(dockerfile, override); got != 2 {
			t.Errorf("contrib/k8s/Dockerfile.base applies %s %d times, want exactly 2 (gh and dolt)", override, got)
		}
	}
	if got := strings.Count(dockerfile, `go get "golang.org/x/text@v${XTEXT_VERSION}"`); got != 2 {
		t.Errorf("contrib/k8s/Dockerfile.base applies the x/text override %d times, want exactly 2 (gh and Dolt)", got)
	}
	// Only dolt: gh links no thrift package, so a `go get` there would raise a
	// go.mod line no `go version -m` assertion could confirm.
	if got := strings.Count(dockerfile, `go get "github.com/apache/thrift@v${THRIFT_VERSION}"`); got != 1 {
		t.Errorf("contrib/k8s/Dockerfile.base applies the thrift override %d times, want exactly 1 (Dolt only)", got)
	}

	for _, forbidden := range []string{
		"apt-get install -y --no-install-recommends gh",
		`/tmp/install-dolt-archive.sh "${DOLT_VERSION}"`,
		"libicu74",
		"-tags=timetzdata",
	} {
		if strings.Contains(dockerfile, forbidden) {
			t.Errorf("contrib/k8s/Dockerfile.base still installs vulnerable prebuilt tool via %q", forbidden)
		}
	}
}

func TestAgentImageRebuildsBDAndGCWithPatchedGRPC(t *testing.T) {
	const (
		// FORK DIVERGENCE — FORK-FIRST BRIDGE (ga-zzcjs; supersedes the
		// ADR-0026 C5 upstream bridge). The image builds bd from the
		// Voxist/beads fork tip (schema 0067 plus fork-only fixes; the
		// 0066->0067 store migration was rehearsed and cut over
		// deliberately). go.mod links BD_LIB_REF — the fork tip's newest
		// upstream ancestor — because fork commits do not resolve on the
		// module path. These values move together with deps.env and go.mod;
		// a published release >= 0067 on a module-resolvable repo retires
		// the bridge (one change: BD_VERSION=tag, drop the refs). Note the
		// existing upstream v1.2.2 TAG is not such a release: it sits on a
		// different lineage at schema 0053.
		// BD_VERSION itself is NOT diverged — the pinned commit declares 1.2.2.
		bdSourceRef    = "73a5bdc65b5fa9cb384683a47e579efa61bd999b"
		bdSourceSHA256 = "f572a92ebaf5d0acde21e685fcafe0178e7f5a158b56e437f161a65bdcb52fdb"
		bdBuild        = "73a5bdc65"
		bdBranch       = "HEAD"
		grpcVersion    = "1.83.2"
		// Floors, not exact pins: each must be >= what the pinned source
		// resolves, because pinning BELOW that is a silent downgrade
		// (ga-0emb8). x/text is 0.41.0 in both images now: the x/crypto floor
		// drags x/text there in bd's, gh's and dolt's module graphs alike.
		xtextVersion = "0.41.0"
		// x/crypto is 0.56.0, not 0.55.0. Three advisories reach crypto/ssh,
		// which this image links: CVE-2026-56854 (CRITICAL) is fixed in
		// 0.55.0, but CVE-2026-78662 and CVE-2026-56855 (SSH channel-deadlock
		// DoS, both published 2026-09-02) need 0.56.0. x/mod needs 0.40.0 for
		// CVE-2026-56864 and CVE-2026-56865 (sumdb). The pinned source now
		// declares x/crypto 0.56.0 and x/mod 0.40.0 itself, so these sit at
		// parity with it rather than lifting it -- and holding them any lower
		// would silently DOWNGRADE through `go get` (ga-0emb8).
		xcryptoVersion = "0.56.0"
		xmodVersion    = "0.40.0"
		// CVE-2026-43871 (HIGH, TCompactProtocol varint byte-count DoS, fixed
		// 0.24.0). bd embeds dolt as a library and reaches thrift through the
		// parquet writer; the pinned source still resolves 0.23.0, so this is
		// the one floor that remains a genuine lift.
		thriftVersion = "0.24.0"
	)

	root := repoRoot(t)
	env := readDotenv(t, root+"/deps.env")
	// The image stamps -X main.Version=${BD_VERSION} onto source fetched at
	// BD_SOURCE_REF, and the Dockerfile's own `grep Version = "${bd_version}"
	// cmd/bd/version.go` fails the build if those two name different releases.
	// Assert it here rather than discovering it in a docker build CI may not run.
	//
	// Upstream anchors this on BD_CURRENT_VERSION, because upstream's
	// BD_SOURCE_REF tracks BD_CURRENT_REF and BD_VERSION is a separate role —
	// the published tarball CI installs, which legitimately lags a
	// bleeding-edge cell pinned to a commit with no release. Under the
	// fork-first bridge (ga-zzcjs) that identity does not hold: BD_SOURCE_REF
	// is the Voxist/beads fork tip, which tracks NOTHING in the matrix
	// (TestBDVersionPins skips the unified-pin assertion whenever a bridge ref
	// is set), and BD_VERSION is precisely the version string that pinned
	// source declares. So BD_VERSION is this fork's correct anchor for exactly
	// upstream's reason: anchor on whatever names the version the pinned
	// SOURCE declares. Re-anchor on BD_CURRENT_VERSION when the bridge exits.
	bdVersion := env["BD_VERSION"]
	if bdVersion != "v1.2.2" {
		t.Fatalf("deps.env BD_VERSION = %q, want v1.2.2 for the pinned source build", bdVersion)
	}

	dockerfile := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	for _, want := range []string{
		"ARG BD_VERSION=" + bdVersion,
		"ARG BD_SOURCE_REF=" + bdSourceRef,
		"ARG BD_SOURCE_SHA256=" + bdSourceSHA256,
		"ARG BD_BUILD=" + bdBuild,
		"ARG BD_BRANCH=" + bdBranch,
		"ARG GRPC_VERSION=" + grpcVersion,
		"ARG XTEXT_VERSION=" + xtextVersion,
		"ARG XCRYPTO_VERSION=" + xcryptoVersion,
		"ARG XMOD_VERSION=" + xmodVersion,
		"ARG THRIFT_VERSION=" + thriftVersion,
		"ARG BD_REPO=Voxist/beads",
		`https://github.com/${BD_REPO}/archive/${BD_SOURCE_REF}.tar.gz`,
		`echo "${BD_SOURCE_SHA256}  /tmp/bd-source.tar.gz" | sha256sum --check --strict`,
		`grep -Fq "Version = \"${bd_version}\"" cmd/bd/version.go`,
		`go get "google.golang.org/grpc@v${GRPC_VERSION}"`,
		`go get "golang.org/x/text@v${XTEXT_VERSION}"`,
		`go get "golang.org/x/crypto@v${XCRYPTO_VERSION}"`,
		`go get "golang.org/x/mod@v${XMOD_VERSION}"`,
		`go get "github.com/apache/thrift@v${THRIFT_VERSION}"`,
		`go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep golang.org/x/crypto v${XCRYPTO_VERSION} "`,
		`go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep golang.org/x/mod v${XMOD_VERSION} "`,
		`CGO_ENABLED=1 go build`,
		`-tags="gms_pure_go"`,
		`-X main.Version=${bd_version}`,
		`-X main.Build=${BD_BUILD}`,
		`-X main.Commit=${BD_SOURCE_REF}`,
		`-X main.Branch=${BD_BRANCH}`,
		`COPY --from=bd-builder /out/bd /usr/local/bin/bd`,
		`CGO_ENABLED=0 go build -o gc ./cmd/gc`,
		`RUN gc version`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("contrib/k8s/Dockerfile.agent missing %q", want)
		}
	}
	if got := strings.Count(dockerfile, `go get "google.golang.org/grpc@v${GRPC_VERSION}"`); got != 1 {
		t.Errorf("contrib/k8s/Dockerfile.agent applies the bd grpc override %d times, want exactly 1", got)
	}
	if got := strings.Count(dockerfile, `go get "golang.org/x/text@v${XTEXT_VERSION}"`); got != 1 {
		t.Errorf("contrib/k8s/Dockerfile.agent applies the bd x/text override %d times, want exactly 1", got)
	}
	for _, override := range []string{
		`go get "golang.org/x/crypto@v${XCRYPTO_VERSION}"`,
		`go get "golang.org/x/mod@v${XMOD_VERSION}"`,
		`go get "github.com/apache/thrift@v${THRIFT_VERSION}"`,
	} {
		if got := strings.Count(dockerfile, override); got != 1 {
			t.Errorf("contrib/k8s/Dockerfile.agent applies %s %d times, want exactly 1", override, got)
		}
	}
	if strings.Contains(dockerfile, "COPY bd /usr/local/bin/bd") {
		t.Error("contrib/k8s/Dockerfile.agent still copies the vulnerable prebuilt bd binary")
	}
	baseImageArg := strings.Index(dockerfile, "ARG BASE_IMAGE=")
	firstStage := strings.Index(dockerfile, "FROM ")
	if baseImageArg < 0 || firstStage < 0 || baseImageArg > firstStage {
		t.Error("contrib/k8s/Dockerfile.agent must declare BASE_IMAGE globally before its first FROM")
	}

	goMod := readFile(t, root, "go.mod")
	// grpc's go.mod requirement is asserted by the FLOOR table below, alongside
	// x/text, x/crypto and x/mod, rather than by counting an exact
	// "module vVERSION" substring. The count was brittle in both directions: it
	// asserted equality where the security requirement is >= (so a future bump
	// past the floor would fail a guard meant to enforce a minimum), and a raw
	// substring match does not respect module-name boundaries the way
	// goModVersion's exact field comparison does — go.mod also carries
	// go.opentelemetry.io/.../google.golang.org/grpc/otelgrpc.
	// bd resolves x/text through its own module graph, not gc's, so the gc binary
	// and the bd binary each need their own floor. This is the gc side.
	//
	// The FLOOR is the security requirement; the image ARGs above are the
	// versions bd's graph actually resolves to, and those must be >= the floor,
	// not equal to it (the Dockerfile asserts its ARGs by exact `go version -m`
	// match, so an ARG has to track the resolved value). Conflating the two is
	// how the x/text override silently became a downgrade: see ga-0emb8.
	const (
		xtextFloor = "0.40.0" // CVE-2026-56852
		// 0.56.0, not 0.55.0: 0.55.0 clears only CVE-2026-56854 (CRITICAL).
		// CVE-2026-78662 and CVE-2026-56855 (SSH channel-deadlock DoS,
		// published 2026-09-02) are fixed in 0.56.0. A floor left at 0.55.0
		// would accept a downgrade to a version this repo has already
		// established is insufficient.
		xcryptoFloor = "0.56.0" // CVE-2026-56854, CVE-2026-78662, CVE-2026-56855
		xmodFloor    = "0.40.0" // CVE-2026-56864 (HIGH)
		thriftFloor  = "0.24.0" // CVE-2026-43871 (HIGH)
		grpcFloor    = "1.83.2" // CVE-2026-84445 (HIGH, xDS server DoS)
	)
	for _, f := range []struct{ module, floor, cve string }{
		{"golang.org/x/text", xtextFloor, "CVE-2026-56852"},
		{"golang.org/x/crypto", xcryptoFloor, "CVE-2026-56854"},
		{"golang.org/x/mod", xmodFloor, "CVE-2026-56864"},
		{"google.golang.org/grpc", grpcFloor, "CVE-2026-84445"},
	} {
		if got := goModVersion(t, goMod, f.module); !semverAtLeast(got, parseModuleSemver(t, "v"+f.floor)) {
			t.Errorf("go.mod pins %s %v, want >= v%s so the gc binary clears %s", f.module, got, f.floor, f.cve)
		}
	}
	// And the image ARGs must not sit BELOW the same floors.
	for _, f := range []struct{ name, arg, floor string }{
		{"XTEXT_VERSION", xtextVersion, xtextFloor},
		{"XCRYPTO_VERSION", xcryptoVersion, xcryptoFloor},
		{"XMOD_VERSION", xmodVersion, xmodFloor},
		{"THRIFT_VERSION", thriftVersion, thriftFloor},
	} {
		if !semverAtLeast(parseModuleSemver(t, "v"+f.arg), parseModuleSemver(t, "v"+f.floor)) {
			t.Errorf("Dockerfile.agent %s=%s is below the security floor v%s — an override that pins below what the source resolves is a downgrade", f.name, f.arg, f.floor)
		}
	}

	workflow := readFile(t, root, ".github/workflows/container-scan.yml")
	if !strings.Contains(workflow, "CGO_ENABLED=0 go build -o gc ./cmd/gc") {
		t.Error("container scan must build gc with the release's portable CGO_ENABLED=0 configuration")
	}
}

func TestMCPMailImagePinsPatchedPythonDependencies(t *testing.T) {
	root := repoRoot(t)
	input := readFile(t, root, ".github/requirements/mcp-agent-mail.in")
	for _, want := range []string{
		"gitpython>=3.1.59",
		"pillow>=12.3.0",
		"aiohttp>=3.14.3",
	} {
		if !strings.Contains(input, want) {
			t.Errorf("mcp-agent-mail input requirements missing security floor %q", want)
		}
	}
	// cryptography is capped by a transitive constraint, so its floor lives in
	// the overrides file rather than the input requirements.
	overrides := readFile(t, root, ".github/requirements/mcp-agent-mail.overrides.txt")
	if want := "cryptography>=50.0.0"; !strings.Contains(overrides, want) {
		t.Errorf("mcp-agent-mail overrides missing security floor %q", want)
	}

	// The lock is hash-pinned and regenerated by the `uv pip compile` command in
	// its own header, and each resolved version must stay at or above the floor
	// above. Assert that as an inequality rather than pinning the exact resolved
	// string: `uv pip compile` takes the newest release satisfying the floor, so
	// an equality pin inverts the moment a newer patched release is locked -- a
	// strictly MORE patched lock fails, and the failure reads as "missing patched
	// dependency", which tells the maintainer to downgrade. The inequality still
	// catches what the pin was there for, because bumping a floor without
	// regenerating the lock leaves the lock BELOW the floor (and its stale hashes
	// fail the image build), which fails this check too.
	lock := readFile(t, root, ".github/requirements/mcp-agent-mail.txt")
	for _, dep := range []struct {
		name  string
		floor string
	}{
		{"gitpython", "3.1.59"},
		{"pillow", "12.3.0"},
		{"aiohttp", "3.14.3"},
		{"cryptography", "50.0.0"},
	} {
		locked, ok := lockedVersion(lock, dep.name)
		if !ok {
			t.Errorf("mcp-agent-mail hashed lock has no %s== pin", dep.name)
			continue
		}
		have, ok := parsePyVersion(locked)
		if !ok {
			t.Errorf("mcp-agent-mail hashed lock pins %s==%s, which this check cannot compare against the floor %s; extend parsePyVersion rather than waiving the floor", dep.name, locked, dep.floor)
			continue
		}
		want, ok := parsePyVersion(dep.floor)
		if !ok {
			t.Errorf("security floor %s for %s is not a comparable PyPI release version", dep.floor, dep.name)
			continue
		}
		if !semverAtLeast(have, want) {
			t.Errorf("mcp-agent-mail hashed lock pins %s==%s, below the security floor %s", dep.name, locked, dep.floor)
		}
	}
}

// TestMCPMailImageUpgradesPatchedOSPackages guards the --only-upgrade list in
// Dockerfile.mail. The base image is pinned by digest, so an OS-package CVE is only
// cleared by naming the package here, and Trivy reports every binary package of a
// source separately: dropping one name leaves that package on the vulnerable version
// and the scan red, with the other eight looking like the whole fix.
func TestMCPMailImageUpgradesPatchedOSPackages(t *testing.T) {
	dockerfile := readFile(t, repoRoot(t), "contrib/k8s/Dockerfile.mail")

	upgrade, _, ok := strings.Cut(dockerfile, "&& apt-get install -y --no-install-recommends \\")
	if !ok {
		t.Fatal("contrib/k8s/Dockerfile.mail has no plain apt-get install stanza to bound the --only-upgrade list")
	}
	if !strings.Contains(upgrade, "--only-upgrade") {
		t.Fatal("contrib/k8s/Dockerfile.mail no longer upgrades any pinned-base OS package")
	}

	for _, pkg := range []string{
		// openssl / systemd set, already present.
		"libcap2", "libssl3t64", "libsystemd0", "libudev1", "openssl", "openssl-provider-legacy",
		// util-linux set, CVE-2026-53615, fixed in 2.41.5-0+deb13u1.
		"bsdutils", "libblkid1", "liblastlog2-2", "libmount1", "libsmartcols1",
		"libuuid1", "login", "mount", "util-linux",
		// Added by #186 and load-bearing: these three are what clear
		// CVE-2026-89161 (pcre2) and CVE-2026-11822/11824 (sqlite3) on
		// gc-mcp-mail. They shipped without an assertion, so nothing stopped a
		// later edit from dropping them and turning the scan red again.
		"gzip", "libpcre2-8-0", "libsqlite3-0",
	} {
		if !strings.Contains(upgrade, "\n    "+pkg+" \\") {
			t.Errorf("contrib/k8s/Dockerfile.mail --only-upgrade list missing %q", pkg)
		}
	}
}

// parsePyVersion parses a PyPI release version into three comparable numeric
// components. PyPI versions are NOT Go module semver, so parseModuleSemver is
// the wrong tool: stable releases legitimately carry two components (the
// cryptography project has shipped 2.9, 3.0 and 3.4) or four (GitPython
// 0.3.2.1). Absent trailing components are zero, so "50.0" compares equal to
// "50.0.0".
//
// A version carrying a prerelease or local suffix ("3.9.0rc0", "5.4.0.dev0")
// is REJECTED rather than truncated to its numeric prefix. Truncating would
// let 3.9.0rc0 satisfy a floor of 3.9.0, which is backwards for a security
// floor; `uv pip compile` does not resolve prereleases by default, so
// rejecting fails closed on a case that should not arise.
func parsePyVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimSpace(v), ".")
	// strings.Split never returns an empty slice, so len(parts) >= 1 always;
	// the empty-string case is rejected by the Atoi below rather than by a
	// length check. An unreachable guard is indistinguishable from a working
	// one, so the invariant is stated here instead of pretended at.
	if len(parts) > 4 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		if i < 3 {
			out[i] = n
		} else if n != 0 {
			// A non-zero fourth component ("0.3.2.1") orders after the
			// three-component release but cannot be represented here.
			return out, false
		}
	}
	return out, true
}

// TestParsePyVersionHandlesRealPyPIShapes guards parsePyVersion against the
// version forms these four projects have actually shipped. The two-component
// case is the one that motivated the parser: cryptography's 2.9/3.0/3.4 are
// stable releases, so a future 51.0 must compare cleanly against a three-
// component floor rather than aborting the check.
func TestParsePyVersionHandlesRealPyPIShapes(t *testing.T) {
	for _, c := range []struct {
		in   string
		ok   bool
		want [3]int
	}{
		{"3.1.62", true, [3]int{3, 1, 62}},
		{"12.3.0", true, [3]int{12, 3, 0}},
		{"50.0", true, [3]int{50, 0, 0}},
		{"3.0", true, [3]int{3, 0, 0}},
		{"3.1.62.0", true, [3]int{3, 1, 62}},
		{"0.3.2.1", false, [3]int{}},
		{"3.9.0rc0", false, [3]int{}},
		{"5.4.0.dev0", false, [3]int{}},
		{"0.3.0-beta2", false, [3]int{}},
		{"", false, [3]int{}},
	} {
		got, ok := parsePyVersion(c.in)
		if ok != c.ok {
			t.Errorf("parsePyVersion(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("parsePyVersion(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	// A two-component release must satisfy an equal three-component floor, and
	// a lower one must not.
	floor, ok := parsePyVersion("50.0.0")
	if !ok {
		t.Fatal("floor 50.0.0 must parse")
	}
	if have, _ := parsePyVersion("50.0"); !semverAtLeast(have, floor) {
		t.Error("50.0 must satisfy floor 50.0.0")
	}
	if have, _ := parsePyVersion("49.0"); semverAtLeast(have, floor) {
		t.Error("49.0 must not satisfy floor 50.0.0")
	}
}

// lockedVersion returns the version a hash-pinned `uv pip compile` lock resolved
// for pkg, matching the `name==version \` line the compiler emits at column 0.
// The second result reports whether such a line was found.
//
// The "==" is part of the matched prefix, which is what makes this safe against
// a package whose name is a prefix of another: "gitpython-ext==9.9.9" does not
// match "gitpython==" because the byte after the name is "-", not "=".
//
// It returns the FIRST match, which is the one place this helper could answer
// wrongly rather than fail: a lock carrying two marker-differentiated pins for
// one package ("foo==1.0 ; python_version < '3.9'" and "foo==2.0 ; ...") would
// be judged on whichever came first. That cannot arise from the command in this
// lock's header -- `uv pip compile --python-version 3.12 --python-platform
// linux` resolves a single version per package, and the committed lock carries
// no extras forms, environment markers or duplicate pins across any of its
// pins. Recorded because it is the only silently-wrong branch here; every other
// malformed input fails the check rather than passing it.
func lockedVersion(lock, pkg string) (string, bool) {
	for _, line := range strings.Split(lock, "\n") {
		rest, found := strings.CutPrefix(line, pkg+"==")
		if !found {
			continue
		}
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "\\")), true
	}
	return "", false
}

// TestRebuiltToolsAssertPatchedGRPCArtifact guards the artifact-level proof that
// each rebuilt CLI actually embeds the patched grpc module. Text-level ARG/recipe
// checks confirm the build inputs; these `go version -m` assertions are the only
// evidence the produced binary links grpc v${GRPC_VERSION}, so they must not be
// silently removable. bd already had one; gh and dolt now mirror it.
func TestRebuiltToolsAssertPatchedGRPCArtifact(t *testing.T) {
	root := repoRoot(t)

	base := readFile(t, root, "contrib/k8s/Dockerfile.base")
	for _, bin := range []string{"/out/gh", "/out/dolt"} {
		want := `go version -m ` + bin + ` | tr '\t' ' ' | grep -Fq "dep google.golang.org/grpc v${GRPC_VERSION} "`
		if !strings.Contains(base, want) {
			t.Errorf("contrib/k8s/Dockerfile.base must assert %s embeds patched grpc; missing %q", bin, want)
		}
	}

	agent := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	want := `go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep google.golang.org/grpc v${GRPC_VERSION} "`
	if !strings.Contains(agent, want) {
		t.Errorf("contrib/k8s/Dockerfile.agent must assert /out/bd embeds patched grpc; missing %q", want)
	}
}

// TestRebuiltToolsAssertPatchedXTextArtifact mirrors the grpc artifact guard for
// golang.org/x/text (CVE-2026-56852, norm.Iter infinite loop, fixed in 0.39.0).
// Each rebuilt CLI pulled x/text through its own module graph — at the pinned refs
// MVS selects 0.38.0 for gh and 0.36.0 for both dolt and bd — so bumping this repo's
// go.mod fixes only the gc binary. These `go version -m` assertions are the only evidence the three
// rebuilt artifacts actually link the patched module, so they must not be silently
// removable.
func TestRebuiltToolsAssertPatchedXTextArtifact(t *testing.T) {
	root := repoRoot(t)

	base := readFile(t, root, "contrib/k8s/Dockerfile.base")
	for _, bin := range []string{"/out/gh", "/out/dolt"} {
		want := `go version -m ` + bin + ` | tr '\t' ' ' | grep -Fq "dep golang.org/x/text v${XTEXT_VERSION} "`
		if !strings.Contains(base, want) {
			t.Errorf("contrib/k8s/Dockerfile.base must assert %s embeds patched x/text; missing %q", bin, want)
		}
	}

	agent := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	want := `go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep golang.org/x/text v${XTEXT_VERSION} "`
	if !strings.Contains(agent, want) {
		t.Errorf("contrib/k8s/Dockerfile.agent must assert /out/bd embeds patched x/text; missing %q", want)
	}
}

// TestRebuiltToolsAssertPatchedXCryptoAndXModArtifacts mirrors the grpc and
// x/text artifact guards for golang.org/x/crypto and golang.org/x/mod. The
// three rebuilt CLIs each `go get` these floors, and the header comment in
// Dockerfile.base says the post-build `go version -m` check is what turns a
// silently-not-applied floor into a build failure (ga-0emb8). Until this
// test existed only bd carried the two assertions; gh and dolt applied the
// floors with no check, so a downgrade there shipped with a green build.
func TestRebuiltToolsAssertPatchedXCryptoAndXModArtifacts(t *testing.T) {
	root := repoRoot(t)

	// dolt links x/crypto but not x/mod (go version -m lists no x/mod line
	// for it), so its x/mod check is an absence assertion; gh links both.
	for _, mod := range []struct {
		name, arg string
		bins      []string
	}{
		{"x/crypto", "XCRYPTO_VERSION", []string{"/out/gh", "/out/dolt"}},
		{"x/mod", "XMOD_VERSION", []string{"/out/gh"}},
	} {
		base := readFile(t, root, "contrib/k8s/Dockerfile.base")
		for _, bin := range mod.bins {
			want := `go version -m ` + bin + ` | tr '\t' ' ' | grep -Fq "dep golang.org/` + mod.name + ` v${` + mod.arg + `} "`
			if !strings.Contains(base, want) {
				t.Errorf("contrib/k8s/Dockerfile.base must assert %s embeds patched %s; missing %q", bin, mod.name, want)
			}
		}
		if mod.name == "x/mod" {
			want := `! go version -m /out/dolt | tr '\t' ' ' | grep -q "dep golang.org/x/mod "`
			if !strings.Contains(base, want) {
				t.Errorf("contrib/k8s/Dockerfile.base must assert /out/dolt does not link x/mod (it never has; if it starts to, switch to the exact-version form); missing %q", want)
			}
		}
		agent := readFile(t, root, "contrib/k8s/Dockerfile.agent")
		want := `go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep golang.org/` + mod.name + ` v${` + mod.arg + `} "`
		if !strings.Contains(agent, want) {
			t.Errorf("contrib/k8s/Dockerfile.agent must assert /out/bd embeds patched %s; missing %q", mod.name, want)
		}
	}
}

// TestRebuiltToolsAssertPatchedThriftArtifact mirrors the grpc, x/text,
// x/crypto and x/mod artifact guards for github.com/apache/thrift
// (CVE-2026-43871, TCompactProtocol varint byte-count DoS, fixed in 0.24.0;
// CVE-2026-41602, TFramedTransport uint32 overflow, fixed in 0.23.0).
//
// dolt reaches thrift through go.opentelemetry.io/otel/exporters/jaeger v1.17.0
// (which pins the v0.13.1-0.20201008052519 pseudo-version) and through the
// parquet writer; bd embeds dolt as a library and reaches it through the same
// parquet writer, resolving 0.23.0. Neither is reachable from this repo's
// go.mod -- the gc binary is built CGO_ENABLED=0 and links no thrift package --
// so the floor has to be applied inside each source rebuild. These
// `go version -m` assertions are the only evidence the produced binaries link
// the patched module, so they must not be silently removable. gh links no
// thrift package and therefore carries no assertion (ga-rx7nw).
func TestRebuiltToolsAssertPatchedThriftArtifact(t *testing.T) {
	root := repoRoot(t)

	base := readFile(t, root, "contrib/k8s/Dockerfile.base")
	want := `go version -m /out/dolt | tr '\t' ' ' | grep -Fq "dep github.com/apache/thrift v${THRIFT_VERSION} "`
	if !strings.Contains(base, want) {
		t.Errorf("contrib/k8s/Dockerfile.base must assert /out/dolt embeds patched thrift; missing %q", want)
	}

	agent := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	want = `go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep github.com/apache/thrift v${THRIFT_VERSION} "`
	if !strings.Contains(agent, want) {
		t.Errorf("contrib/k8s/Dockerfile.agent must assert /out/bd embeds patched thrift; missing %q", want)
	}
}

// TestTrivyIgnoreCarriesNoThriftWaiver enforces that no path is waived for the
// apache/thrift CVEs the source-rebuild floor now clears. Both carriers (dolt
// and bd) are rebuilt from pinned source with THRIFT_VERSION applied and
// asserted, so a waiver here would let the scan gate mask a regressed rebuild
// instead of proving the floor holds -- the same rule the stdlib guard below
// enforces for the Go-toolchain CVEs (ga-rx7nw).
func TestTrivyIgnoreCarriesNoThriftWaiver(t *testing.T) {
	root := repoRoot(t)

	var doc struct {
		Vulnerabilities []struct {
			ID    string   `yaml:"id"`
			Paths []string `yaml:"paths"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	thriftCVEs := map[string]bool{
		"CVE-2026-41602": true, // fixed in thrift 0.23.0
		"CVE-2026-43871": true, // fixed in thrift 0.24.0
	}
	for _, v := range doc.Vulnerabilities {
		if !thriftCVEs[v.ID] {
			continue
		}
		t.Errorf("%s is waived for %v, but the dolt and bd rebuilds apply and assert THRIFT_VERSION >= 0.24.0; drop the waiver so the scan proves the floor stays effective", v.ID, v.Paths)
	}
}

// TestRebuiltToolsForcePatchedXModules guards the module overrides that replaced the
// gh and Dolt waivers in .trivyignore.yaml. The pinned gh and Dolt sources select
// x/crypto, x/net, x/text and thrift versions Trivy flags, and the grpc-only override
// left them there, which is what kept those paths waived. Dropping either the `go get`
// or its `go version -m` proof would put the vulnerable module back with nothing
// failing, so both halves are asserted here.
func TestRebuiltToolsForcePatchedXModules(t *testing.T) {
	root := repoRoot(t)
	base := readFile(t, root, "contrib/k8s/Dockerfile.base")

	// Versions each override forces, at or above what Trivy names as fixed for the findings it clears.
	//
	// NOTE: these are exact-string matches, not floors, despite the "at or
	// above" wording. Moving an ARG FORWARD therefore fails this test until the
	// literal is updated here too (which is what the #186 xcrypto 0.55.0 ->
	// 0.56.0 bump did), and a DOWNGRADE back to the literal would pass it.
	// Tracked rather than restructured inside a merge.
	for _, arg := range []string{
		"ARG XCRYPTO_VERSION=0.56.0",
		"ARG XNET_VERSION=0.58.0",
		"ARG XTEXT_VERSION=0.41.0",
		"ARG XMOD_VERSION=0.40.0",
		"ARG THRIFT_VERSION=0.24.0",
	} {
		if !strings.Contains(base, arg) {
			t.Errorf("contrib/k8s/Dockerfile.base missing %q", arg)
		}
	}

	// gh needs x/text and x/mod; its pinned source already selects patched x/crypto and x/net.
	// Dolt takes no x/mod override because no x/mod package is linked into its binary.
	ghModules := map[string]string{
		"golang.org/x/text": "XTEXT_VERSION",
		"golang.org/x/mod":  "XMOD_VERSION",
	}
	doltModules := map[string]string{
		"golang.org/x/crypto":      "XCRYPTO_VERSION",
		"golang.org/x/net":         "XNET_VERSION",
		"golang.org/x/text":        "XTEXT_VERSION",
		"github.com/apache/thrift": "THRIFT_VERSION",
	}
	// Each `go get` is looked for inside its own stanza. gh and Dolt share the x/text
	// override verbatim, so a file-wide search lets one stand in for the other and a
	// dropped override reads as present here, failing only in the image build.
	ghStart := strings.Index(base, "WORKDIR /src/gh")
	doltStart := strings.Index(base, "WORKDIR /src/dolt")
	if ghStart < 0 || doltStart <= ghStart {
		t.Fatal("contrib/k8s/Dockerfile.base has no WORKDIR /src/gh stanza ahead of the Dolt one")
	}
	ghStanza := base[ghStart:doltStart]
	doltStanza := base[doltStart:]
	if next := strings.Index(doltStanza, "\nFROM "); next > 0 {
		doltStanza = doltStanza[:next]
	}
	stanzas := map[string]string{"/out/gh": ghStanza, "/out/dolt": doltStanza}

	for bin, modules := range map[string]map[string]string{"/out/gh": ghModules, "/out/dolt": doltModules} {
		for module, arg := range modules {
			get := `"` + module + `@v${` + arg + `}"`
			if !strings.Contains(stanzas[bin], get) {
				t.Errorf("contrib/k8s/Dockerfile.base must override %s inside the %s build stanza; missing %q", module, bin, get)
			}
			assert := `go version -m ` + bin + ` | tr '\t' ' ' | grep -Fq "dep ` + module + ` v${` + arg + `} "`
			if !strings.Contains(base, assert) {
				t.Errorf("contrib/k8s/Dockerfile.base must assert %s embeds patched %s; missing %q", bin, module, assert)
			}
		}
	}

	// Inside the gh stanza the x/mod get has to run after the x/text one. A later
	// `go get` naming a version below what an earlier one dragged in is a downgrade,
	// and it takes the earlier module with it: measured against the pinned source,
	// with XTEXT_VERSION at 0.39.0 the reversed order selects x/mod v0.38.0, under
	// the fixed version. The two orders agree at the versions pinned today, so this
	// is what keeps the next bump from recreating that shape.
	xtextGet := strings.Index(ghStanza, `"golang.org/x/text@v${XTEXT_VERSION}"`)
	xmodGet := strings.Index(ghStanza, `"golang.org/x/mod@v${XMOD_VERSION}"`)
	if xtextGet < 0 || xmodGet < 0 {
		t.Fatal("gh stanza is missing the x/text or the x/mod override")
	}
	if xmodGet < xtextGet {
		t.Error("gh stanza runs the x/mod override ahead of x/text; the newest constraint goes last, so a lower x/text pin cannot downgrade x/mod out from under its assertion")
	}
}

// TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools enforces that the rebuilt-from-
// source tools (bd, dolt, gh) carry no Go-stdlib CVE waiver, and no module waiver
// beyond the reviewed set below. The image build rebuilds them with the Go 1.26.8
// toolchain, which fixes every stdlib CVE listed, so a waiver on those paths would
// let the scan gate keep masking a regressed rebuild instead of proving the fix holds.
//
// The rebuilds are no longer grpc-only: they apply and assert x/text, x/crypto, x/mod
// and thrift floors too, so these same paths must carry no MODULE waiver either --
// enforced by TestTrivyIgnoreDropsModuleWaiversForRebuiltTools. gc's module waivers are
// enforced by TestTrivyIgnoreDropsGCModuleWaiversPastThreshold, and kubectl's, the last
// prebuilt binary, by TestTrivyIgnoreDropsKubectlWaiversPastPinnedVersion.
//
// The reviewed set is the one finding the pins do NOT clear, carried over from main's
// time-boxed bridge and held to exactly the paths the scan reported;
// TestTrivyIgnoreKeepsReviewedBridgeEntries pins its horizon and statement. Holding it
// here as both a floor and a ceiling is what keeps the set from growing back without a
// deliberate edit -- the shape that produced the 2026-09-07 expiry cliff (ga-elgvf).
func TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools(t *testing.T) {
	root := repoRoot(t)

	var doc struct {
		Vulnerabilities []struct {
			ID    string   `yaml:"id"`
			Paths []string `yaml:"paths"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	rebuiltPaths := map[string]bool{
		"usr/local/bin/bd":   true,
		"usr/local/bin/dolt": true,
		"usr/bin/gh":         true,
	}
	stdlibCVEs := map[string]bool{
		"CVE-2026-33811": true, "CVE-2026-33814": true, "CVE-2026-39820": true,
		"CVE-2026-39822": true, "CVE-2026-39823": true, "CVE-2026-39825": true,
		"CVE-2026-39826": true, "CVE-2026-39836": true, "CVE-2026-42499": true,
		"CVE-2026-42504": true, "CVE-2026-27145": true,
		// kubectl-only stdlib CVEs, fixed in Go 1.26.6.
		"CVE-2026-33818": true, "CVE-2026-56853": true, "CVE-2026-56858": true,
		"CVE-2026-56859": true, "CVE-2026-56860": true, "CVE-2026-56862": true,
	}
	// Waivers that survive, checked as present so an entry cannot be dropped without
	// a deliberate edit here, and as the only rebuilt-path entries allowed, so the set
	// cannot grow without one either. The gc path of the grpc entry is governed by
	// TestTrivyIgnoreDropsGCModuleWaiversPastThreshold instead, so it is not listed.
	//
	// Only CVE-2026-84445 is left. grpc fixes it in 1.82.2 and 1.83.2, and every pin
	// here is 1.83.1 -- past CVE-2026-84304, which 1.83.1 does fix, but short of the
	// 1.83 line's fix for this one. CVE-2026-84304 (grpc 1.83.1), CVE-2026-43871
	// (thrift 0.24.0 in Dockerfile.base and Dockerfile.agent) and CVE-2026-56852
	// (kubectl v1.37.0, x/text 0.40.0) are cleared by the pins and must stay unwaived;
	// TestTrivyIgnoreCarriesNoThriftWaiver and
	// TestTrivyIgnoreDropsKubectlWaiversPastPinnedVersion enforce two of those.
	reviewedWaivers := map[string]map[string]bool{
		"CVE-2026-84445": {
			"usr/bin/gh":         true,
			"usr/local/bin/dolt": true,
			"usr/local/bin/bd":   true,
		},
	}
	foundReviewed := map[string]map[string]bool{}

	for _, v := range doc.Vulnerabilities {
		for _, p := range v.Paths {
			if stdlibCVEs[v.ID] && rebuiltPaths[p] {
				t.Errorf("%s still waives rebuilt tool %q for a Go-stdlib CVE the Go 1.26.8 rebuild clears; drop the path so the scan proves the fix stays effective", v.ID, p)
			}
			if allowedPaths, ok := reviewedWaivers[v.ID]; ok && allowedPaths[p] {
				if foundReviewed[v.ID] == nil {
					foundReviewed[v.ID] = map[string]bool{}
				}
				foundReviewed[v.ID][p] = true
				continue
			}
			if rebuiltPaths[p] {
				t.Errorf("%s waives rebuilt tool %q; Dockerfile.base and Dockerfile.agent force the patched modules into the gh, Dolt and bd builds and assert them on the produced artifact, so move the floor forward in that build instead of waiving the path", v.ID, p)
			}
		}
	}
	for cve, paths := range reviewedWaivers {
		for path := range paths {
			if !foundReviewed[cve][path] {
				t.Errorf(".trivyignore.yaml must retain the reviewed %s waiver for %s until its pin moves past the fixed version", cve, path)
			}
		}
	}
}

// goModVersion returns the [major, minor, patch] version go.mod pins for module,
// reading the require directive directly so the guard tests never drift from the
// tree's actual module graph. Replace directives are ignored.
func goModVersion(t *testing.T, goMod, module string) [3]int {
	t.Helper()
	for _, line := range strings.Split(goMod, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "replace ") || strings.Contains(line, "=>") {
			continue
		}
		line = strings.TrimPrefix(line, "require ")
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == module && strings.HasPrefix(fields[1], "v") {
			return parseModuleSemver(t, fields[1])
		}
	}
	t.Fatalf("go.mod does not pin %s", module)
	return [3]int{}
}

// parseModuleSemver parses a "vMAJOR.MINOR.PATCH" module version into comparable parts.
func parseModuleSemver(t *testing.T, v string) [3]int {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		t.Fatalf("version %q is not vMAJOR.MINOR.PATCH", v)
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("parsing %q component %q: %v", v, p, err)
		}
		out[i] = n
	}
	return out
}

// semverAtLeast reports whether have is greater than or equal to want.
func semverAtLeast(have, want [3]int) bool {
	for i := range have {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

// TestTrivyIgnoreDropsGCModuleWaiversPastThreshold enforces that no usr/local/bin/gc
// x/net, x/crypto, or grpc CVE waiver outlives the go.mod bump that fixes it. Unlike the
// rebuilt tools (bd, dolt, gh), gc is built straight from this module, so a waiver on a
// gc path is only honest while go.mod still pins a vulnerable version. Each CVE records
// the module and the first version that fixes it (taken from the waiver's own removal
// text); once go.mod reaches that version the gc path must be dropped, or the container
// scan would stay green without proving the gc binary is clean.
func TestTrivyIgnoreDropsGCModuleWaiversPastThreshold(t *testing.T) {
	root := repoRoot(t)

	type modFix struct {
		module     string
		fixVersion string
	}
	gcModuleCVEs := map[string]modFix{
		// golang.org/x/net http2, fixed in 0.53.0.
		"CVE-2026-33814": {"golang.org/x/net", "v0.53.0"},
		// golang.org/x/net HTML/idna, fixed only in 0.55.0.
		"CVE-2026-25680": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-25681": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-27136": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-39821": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-42502": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-42506": {"golang.org/x/net", "v0.55.0"},
		// golang.org/x/crypto/ssh*, fixed in 0.52.0.
		"CVE-2026-39827": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39828": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39829": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39830": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39831": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39832": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39835": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-42508": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-46595": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-46597": {"golang.org/x/crypto", "v0.52.0"},
		// google.golang.org/grpc. CVE-2026-84445 is fixed in 1.82.2 on the 1.82 line
		// and 1.83.2 on the 1.83 line go.mod is on, so 1.83.1 clears only the first.
		"CVE-2026-84304": {"google.golang.org/grpc", "v1.83.1"},
		"CVE-2026-84445": {"google.golang.org/grpc", "v1.83.2"},
	}

	goMod := readFile(t, root, "go.mod")
	have := map[string][3]int{
		"golang.org/x/net":       goModVersion(t, goMod, "golang.org/x/net"),
		"golang.org/x/crypto":    goModVersion(t, goMod, "golang.org/x/crypto"),
		"google.golang.org/grpc": goModVersion(t, goMod, "google.golang.org/grpc"),
	}

	var doc struct {
		Vulnerabilities []struct {
			ID    string   `yaml:"id"`
			Paths []string `yaml:"paths"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	for _, v := range doc.Vulnerabilities {
		fix, tracked := gcModuleCVEs[v.ID]
		if !tracked {
			continue
		}
		waivesGC := false
		for _, p := range v.Paths {
			if p == "usr/local/bin/gc" {
				waivesGC = true
			}
		}
		if !waivesGC {
			continue
		}
		if semverAtLeast(have[fix.module], parseModuleSemver(t, fix.fixVersion)) {
			t.Errorf("%s still waives usr/local/bin/gc but go.mod pins %s >= %s, which fixes it; drop the gc path so the container scan proves the gc binary is clean", v.ID, fix.module, fix.fixVersion)
		}
	}
}

// TestGoModPinsXModPastGCFinding guards the gc half of the same container-scan
// finding the Dockerfile overrides cover for gh. gc is built straight from this
// module rather than from pinned third-party source, so no `go get` in a Dockerfile
// can move it: the only floor is go.mod's own pin, and dropping that pin back below
// the fixed version would put the vulnerable module into usr/local/bin/gc with
// nothing in the build failing.
func TestGoModPinsXModPastGCFinding(t *testing.T) {
	// The first version Trivy names as fixed for the x/mod findings on usr/local/bin/gc.
	const xmodFixVersion = "v0.40.0"

	goMod := readFile(t, repoRoot(t), "go.mod")
	have := goModVersion(t, goMod, "golang.org/x/mod")
	if !semverAtLeast(have, parseModuleSemver(t, xmodFixVersion)) {
		t.Errorf("go.mod pins golang.org/x/mod below %s, so the gc binary carries the flagged module; raise the pin rather than waiving the gc path", xmodFixVersion)
	}
}

// TestTrivyIgnoreKeepsReviewedBridgeEntries pins the entries carried over from main's
// time-boxed waiver bridge: the findings the pins do not clear. CVE-2026-84445 was
// published against the very grpc version every pin here moves to (1.83.1; the 1.83
// line fixes it in 1.83.2), and the GitPython findings sit in the mail image's
// requirements, which no rebuild touches. Each is held to the exact paths or purls the
// scan reported, to the bridge's own 2026-09-21 horizon, and to a statement naming the
// fixed version and the pin that has to move. The bridge's other entries are what the
// pins cleared -- CVE-2026-84304 by grpc 1.83.1, CVE-2026-43871 by thrift 0.24.0,
// CVE-2026-56852 by kubectl v1.37.0 -- and the rebuilt-path guard above, the thrift
// guard and the kubectl guard are what keep them from coming back.
func TestTrivyIgnoreKeepsReviewedBridgeEntries(t *testing.T) {
	root := repoRoot(t)

	var doc struct {
		Vulnerabilities []struct {
			ID        string   `yaml:"id"`
			Paths     []string `yaml:"paths"`
			Purls     []string `yaml:"purls"`
			ExpiredAt string   `yaml:"expired_at"`
			Statement string   `yaml:"statement"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	const bridgeHorizon = "2026-09-21"

	toSet := func(vals ...string) map[string]bool {
		m := make(map[string]bool, len(vals))
		for _, v := range vals {
			m[v] = true
		}
		return m
	}

	type wantEntry struct {
		id         string
		paths      map[string]bool
		purls      map[string]bool
		substrings []string
	}
	wantEntries := []wantEntry{
		{
			id: "CVE-2026-84445",
			// usr/local/bin/gc is deliberately ABSENT. #186 raised go.mod's
			// google.golang.org/grpc to 1.83.2, which fixes this CVE, and
			// TestTrivyIgnoreDropsGCModuleWaiversPastThreshold -- which derives
			// the threshold from go.mod rather than hardcoding it -- then
			// requires the gc path be dropped. That derived assertion is the
			// authoritative one; this list is a snapshot, so it follows.
			// The three rebuilt-tool paths stay only because
			// TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools still asserts
			// their retention; with GRPC_VERSION now 1.83.2 in both Dockerfiles
			// those rebuilds should be clean too, which makes this whole entry a
			// deletion candidate once that assertion is revisited.
			paths:      toSet("usr/bin/gh", "usr/local/bin/dolt", "usr/local/bin/bd"),
			substrings: []string{"grpc", "1.83.2", "GRPC_VERSION", "go.mod"},
		},
		{id: "CVE-2026-78676", purls: toSet("pkg:pypi/gitpython"), substrings: []string{"gitpython", "3.1.59", "critical"}},
		{id: "CVE-2026-78675", purls: toSet("pkg:pypi/gitpython"), substrings: []string{"gitpython", "3.1.59"}},
		{id: "CVE-2026-78677", purls: toSet("pkg:pypi/gitpython"), substrings: []string{"gitpython", "3.1.59"}},
	}

	byID := map[string][]int{}
	for i, v := range doc.Vulnerabilities {
		byID[v.ID] = append(byID[v.ID], i)
	}

	reviewed := map[string]bool{}
	for _, want := range wantEntries {
		reviewed[want.id] = true
		idxs := byID[want.id]
		if len(idxs) != 1 {
			t.Errorf("%s appears in %d entries, want exactly 1", want.id, len(idxs))
			continue
		}
		v := doc.Vulnerabilities[idxs[0]]
		if v.ExpiredAt != bridgeHorizon {
			t.Errorf("%s expired_at = %q, want the bridge horizon %q it was carried over on", v.ID, v.ExpiredAt, bridgeHorizon)
		}
		if want.paths != nil {
			gotPaths := toSet(v.Paths...)
			for p := range want.paths {
				if !gotPaths[p] {
					t.Errorf("%s missing required path %q", v.ID, p)
				}
			}
			for p := range gotPaths {
				if !want.paths[p] {
					t.Errorf("%s waives unexpected path %q", v.ID, p)
				}
			}
			if len(v.Purls) != 0 {
				t.Errorf("%s sets purls %v on a path-scoped binary finding; want no purls", v.ID, v.Purls)
			}
		}
		if want.purls != nil {
			gotPurls := toSet(v.Purls...)
			for p := range want.purls {
				if !gotPurls[p] {
					t.Errorf("%s missing required purl %q", v.ID, p)
				}
			}
			for p := range gotPurls {
				if !want.purls[p] {
					t.Errorf("%s waives unexpected purl %q", v.ID, p)
				}
			}
			if len(v.Paths) != 0 {
				t.Errorf("%s sets paths %v on a purl-scoped package finding; want no paths, so the purl match alone confines it to gc-mcp-mail", v.ID, v.Paths)
			}
		}
		statement := strings.ToLower(v.Statement)
		for _, sub := range want.substrings {
			if !strings.Contains(statement, strings.ToLower(sub)) {
				t.Errorf("%s statement %q does not name %q", v.ID, v.Statement, sub)
			}
		}
	}

	// An entry that borrows the bridge's date without being listed above skipped this
	// review, so it is a waiver nobody re-measured -- exactly what the 2026-09-07
	// expiry cliff was made of (ga-elgvf).
	for _, v := range doc.Vulnerabilities {
		if !reviewed[v.ID] && v.ExpiredAt == bridgeHorizon {
			t.Errorf("%s expires on the bridge horizon %s but is not a reviewed bridge entry; list it above or give it this file's own horizon", v.ID, bridgeHorizon)
		}
	}
}

// trivyIgnoreWaivers returns the parsed waiver entries of .trivyignore.yaml.
func trivyIgnoreWaivers(t *testing.T) []struct {
	ID    string   `yaml:"id"`
	Paths []string `yaml:"paths"`
} {
	t.Helper()
	var doc struct {
		Vulnerabilities []struct {
			ID    string   `yaml:"id"`
			Paths []string `yaml:"paths"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}
	return doc.Vulnerabilities
}

// TestTrivyIgnoreCarriesNoBRWaiver enforces that usr/local/bin/br is never
// waived. br is the beads_rust CLI (Dicklesworthstone/beads_rust), a Rust
// binary: the pinned v0.1.20 artifact CI installs carries no Go build info at
// all, so trivy's gobinary analyzer skips it and no Go CVE can ever be
// attributed to that path. Reproduce with
//
//	go version -m br  =>  "not a Go executable"
//
// Ten Go-stdlib waivers nevertheless named this path for months, waiving a
// finding that cannot occur and padding the waiver set that produced the
// 2026-09-07 expiry cliff (ga-elgvf). A waiver here is always a mistake: if br
// ever does carry a real finding, the scan must go red so it gets looked at.
func TestTrivyIgnoreCarriesNoBRWaiver(t *testing.T) {
	for _, v := range trivyIgnoreWaivers(t) {
		for _, p := range v.Paths {
			if p == "usr/local/bin/br" {
				t.Errorf("%s waives %q, but br is a Rust binary with no Go build info and can carry no Go CVE; drop the path", v.ID, p)
			}
		}
	}
}

// TestTrivyIgnoreDropsModuleWaiversForRebuiltTools enforces that the
// rebuilt-from-source tools (bd, dolt, gh) carry no x/crypto or x/net module
// waiver. Each rebuild applies XCRYPTO_VERSION and asserts it on the produced
// artifact with `go version -m`, so the shipped binaries provably link
// x/crypto >= 0.55.0 -- past the 0.52.0 that fixes every x/crypto/ssh CVE
// below.
//
// That assertion also settles x/net without a separate floor: golang.org/x/crypto
// v0.55.0 requires golang.org/x/net v0.57.0, so under minimal version selection
// any x/net linked into these binaries is >= 0.57.0, past the 0.53.0/0.55.0
// fixes below. If x/net is not linked at all, there is no finding to waive.
// Either way the waiver is unnecessary, and keeping one would let the scan mask
// a regressed rebuild instead of proving the floor holds (ga-elgvf).
func TestTrivyIgnoreDropsModuleWaiversForRebuiltTools(t *testing.T) {
	rebuiltPaths := map[string]bool{
		"usr/local/bin/bd":   true,
		"usr/local/bin/dolt": true,
		"usr/bin/gh":         true,
	}
	// module CVEs cleared by the asserted x/crypto 0.55.0 floor and the
	// x/net 0.57.0 it selects.
	moduleCVEs := map[string]string{
		"CVE-2026-33814": "golang.org/x/net (fixed 0.53.0)",
		"CVE-2026-25680": "golang.org/x/net (fixed 0.55.0)",
		"CVE-2026-25681": "golang.org/x/net (fixed 0.55.0)",
		"CVE-2026-27136": "golang.org/x/net (fixed 0.55.0)",
		"CVE-2026-39821": "golang.org/x/net (fixed 0.55.0)",
		"CVE-2026-42502": "golang.org/x/net (fixed 0.55.0)",
		"CVE-2026-42506": "golang.org/x/net (fixed 0.55.0)",
		"CVE-2026-46600": "golang.org/x/net (fixed 0.56.0)",
		"CVE-2026-39832": "golang.org/x/crypto (fixed 0.52.0)",
		"CVE-2026-39835": "golang.org/x/crypto (fixed 0.52.0)",
		"CVE-2026-42508": "golang.org/x/crypto (fixed 0.52.0)",
		"CVE-2026-46595": "golang.org/x/crypto (fixed 0.52.0)",
		"CVE-2026-46597": "golang.org/x/crypto (fixed 0.52.0)",
	}

	for _, v := range trivyIgnoreWaivers(t) {
		mod, tracked := moduleCVEs[v.ID]
		if !tracked {
			continue
		}
		for _, p := range v.Paths {
			if rebuiltPaths[p] {
				t.Errorf("%s still waives rebuilt tool %q for %s, which the asserted XCRYPTO_VERSION floor clears; drop the path so the scan proves the fix stays effective", v.ID, p, mod)
			}
		}
	}
}

// TestTrivyIgnoreDropsKubectlWaiversPastPinnedVersion enforces that no
// usr/local/bin/kubectl waiver outlives the KUBECTL_VERSION bump that fixes it.
// kubectl is the last prebuilt (not rebuilt-from-source) binary in the images,
// so its only remedy is the pin in contrib/k8s/Dockerfile.controller. Each CVE
// below records the first kubectl release whose embedded toolchain and modules
// clear it, measured with `go version -m` on the published linux/amd64 binary:
//
//	v1.36.0  go1.26.2  x/net 0.49.0  x/text 0.33.0
//	v1.37.0  go1.26.6  x/net 0.57.0  x/text 0.40.0
//
// Once the pin reaches that release the waiver must go, or the scan would stay
// green without proving the shipped kubectl is clean -- the same rule
// TestTrivyIgnoreDropsGCModuleWaiversPastThreshold applies to gc (ga-elgvf,
// ga-7jqwr).
func TestTrivyIgnoreDropsKubectlWaiversPastPinnedVersion(t *testing.T) {
	root := repoRoot(t)

	// Every CVE the file waived for kubectl at the 2026-09-07 horizon. All are
	// cleared by v1.37.0: the Go-stdlib entries by go1.26.6, CVE-2026-46600 by
	// x/net 0.57.0, CVE-2026-56852 by x/text 0.40.0.
	kubectlCVEs := map[string]string{
		"CVE-2026-33811": "v1.37.0", "CVE-2026-33814": "v1.37.0",
		"CVE-2026-33818": "v1.37.0", "CVE-2026-39820": "v1.37.0",
		"CVE-2026-39822": "v1.37.0", "CVE-2026-39823": "v1.37.0",
		"CVE-2026-39825": "v1.37.0", "CVE-2026-39826": "v1.37.0",
		"CVE-2026-39836": "v1.37.0", "CVE-2026-42499": "v1.37.0",
		"CVE-2026-42504": "v1.37.0", "CVE-2026-27145": "v1.37.0",
		"CVE-2026-56852": "v1.37.0", "CVE-2026-56853": "v1.37.0",
		"CVE-2026-56858": "v1.37.0", "CVE-2026-56859": "v1.37.0",
		"CVE-2026-56860": "v1.37.0", "CVE-2026-56862": "v1.37.0",
		"CVE-2026-25680": "v1.37.0", "CVE-2026-25681": "v1.37.0",
		"CVE-2026-27136": "v1.37.0", "CVE-2026-39821": "v1.37.0",
		"CVE-2026-42502": "v1.37.0", "CVE-2026-42506": "v1.37.0",
		"CVE-2026-46600": "v1.37.0",
	}

	pinned := dockerfileARG(t, readFile(t, root, "contrib/k8s/Dockerfile.controller"), "KUBECTL_VERSION")
	have := parseModuleSemver(t, pinned)

	for _, v := range trivyIgnoreWaivers(t) {
		fixedIn, tracked := kubectlCVEs[v.ID]
		if !tracked {
			continue
		}
		for _, p := range v.Paths {
			if p != "usr/local/bin/kubectl" {
				continue
			}
			if semverAtLeast(have, parseModuleSemver(t, fixedIn)) {
				t.Errorf("%s still waives usr/local/bin/kubectl but Dockerfile.controller pins KUBECTL_VERSION %s >= %s, which clears it; drop the path so the container scan proves the shipped kubectl is clean", v.ID, pinned, fixedIn)
			}
		}
	}
}

// dockerfileARG returns the default value of `ARG name=value` in a Dockerfile.
func dockerfileARG(t *testing.T, dockerfile, name string) string {
	t.Helper()
	for _, line := range strings.Split(dockerfile, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "ARG "+name+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no `ARG %s=` in Dockerfile", name)
	return ""
}

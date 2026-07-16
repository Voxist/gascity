package provenance

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Artifact describes the provenance-relevant state of a repository at build
// time, derived entirely from read-only git queries. It exists so release
// artifact names and version stamps are machine-derived from the ACTUAL
// build commit — never from a base/merge ref a human happened to have in
// mind (the gc-main-20260710-77916fc6c incident: filename said main +
// 77916fc6c, binary embedded an unmerged side-branch commit).
type Artifact struct {
	// HeadSHA is the full commit hash of HEAD — the commit the build will
	// embed as vcs.revision.
	HeadSHA string
	// ShortSHA is the abbreviated (>= 9 chars) form of HeadSHA used in
	// artifact names.
	ShortSHA string
	// Token is the lineage segment of the artifact name: the base branch
	// name when HEAD is an ancestor of BaseRef, otherwise the sanitized
	// current branch name (never the base branch name — that is refused).
	Token string
	// Dirty reports whether the working tree has uncommitted or untracked
	// changes, matching what the Go toolchain will embed as vcs.modified.
	Dirty bool
	// BaseRef is the remote-tracking ref the lineage claim is made against,
	// exactly as supplied (e.g. "Voxist/main").
	BaseRef string
	// BaseSHA is the abbreviated commit BaseRef resolved to at derivation
	// time, so the stamp stays falsifiable after the ref moves.
	BaseSHA string
	// Ahead counts commits on HEAD that are not on BaseRef.
	Ahead int
	// Behind counts commits on BaseRef that are not on HEAD.
	Behind int
}

var tokenSanitizeRe = regexp.MustCompile(`[^A-Za-z0-9._]+`)

// DeriveArtifact inspects the git repository at repoPath and derives the
// provenance facts an artifact name and version stamp are built from.
//
// baseRef must resolve to a remote-tracking ref (refs/remotes/...): a
// lineage claim that does not name its remote is unfalsifiable — this repo's
// `origin` is the upstream, not the fork, so a bare branch name invites the
// exact wrong-remote comparison this package exists to prevent.
//
// The base branch's name is only ever used as the Token when HEAD is an
// ancestor of baseRef. When it is not, and the current branch shares the
// base branch's name (a local `main` diverged from the fork's main), the
// derivation fails outright rather than mint a misleading name.
func DeriveArtifact(repoPath, baseRef string) (Artifact, error) {
	fullRef, err := gitOut(repoPath, "rev-parse", "--symbolic-full-name", "--verify", "--quiet", baseRef)
	if err != nil || fullRef == "" {
		return Artifact{}, fmt.Errorf("base ref %q does not resolve in %q: pass <remote>/<branch> for a fetched remote (see `git remote -v`)", baseRef, repoPath)
	}
	rest, isRemote := strings.CutPrefix(fullRef, "refs/remotes/")
	if !isRemote {
		return Artifact{}, fmt.Errorf("base ref %q resolves to %s, not a remote-tracking ref: the lineage claim must name the remote (e.g. <fork-remote>/main), because local and upstream branches of the same name diverge silently", baseRef, fullRef)
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return Artifact{}, fmt.Errorf("base ref %q (%s) has no branch component", baseRef, fullRef)
	}
	baseToken := sanitizeToken(parts[1])

	headSHA, err := gitOut(repoPath, "rev-parse", "HEAD")
	if err != nil {
		return Artifact{}, fmt.Errorf("resolving HEAD of %q: %w", repoPath, err)
	}
	shortSHA, err := gitOut(repoPath, "rev-parse", "--short=9", "HEAD")
	if err != nil {
		return Artifact{}, fmt.Errorf("abbreviating HEAD of %q: %w", repoPath, err)
	}
	baseSHA, err := gitOut(repoPath, "rev-parse", "--short=9", baseRef)
	if err != nil {
		return Artifact{}, fmt.Errorf("resolving base %q: %w", baseRef, err)
	}

	status, err := gitOut(repoPath, "status", "--porcelain")
	if err != nil {
		return Artifact{}, fmt.Errorf("checking working tree of %q: %w", repoPath, err)
	}
	dirty := status != ""

	counts, err := gitOut(repoPath, "rev-list", "--left-right", "--count", baseRef+"...HEAD")
	if err != nil {
		return Artifact{}, fmt.Errorf("counting divergence from %q: %w", baseRef, err)
	}
	fields := strings.Fields(counts)
	if len(fields) != 2 {
		return Artifact{}, fmt.Errorf("unexpected rev-list --count output %q", counts)
	}
	behind, err := strconv.Atoi(fields[0])
	if err != nil {
		return Artifact{}, fmt.Errorf("parsing behind count %q: %w", fields[0], err)
	}
	ahead, err := strconv.Atoi(fields[1])
	if err != nil {
		return Artifact{}, fmt.Errorf("parsing ahead count %q: %w", fields[1], err)
	}

	token := baseToken
	if ahead > 0 {
		// HEAD is not an ancestor of the base: the artifact must carry the
		// branch it was actually built from, and must never borrow the base
		// branch's name.
		branch, _ := gitOut(repoPath, "symbolic-ref", "--short", "--quiet", "HEAD")
		token = sanitizeToken(branch)
		if token == "" {
			token = "detached"
		}
		if token == baseToken {
			return Artifact{}, fmt.Errorf(
				"HEAD (%s, branch %q) is not an ancestor of %s@%s (+%d/-%d): refusing to name the artifact %q — this is the wrong-remote trap; rebase onto %s or build from a differently-named branch",
				shortSHA, branch, baseRef, baseSHA, ahead, behind, baseToken, baseRef)
		}
	}

	return Artifact{
		HeadSHA:  headSHA,
		ShortSHA: shortSHA,
		Token:    token,
		Dirty:    dirty,
		BaseRef:  baseRef,
		BaseSHA:  baseSHA,
		Ahead:    ahead,
		Behind:   behind,
	}, nil
}

// Name renders the canonical artifact filename: <binary>-<token>-<UTC
// date>-<short sha>[-dirty]. The sha is HEAD's — the commit the binary
// embeds — by construction, and a dirty build is visible in the name so it
// can never masquerade as a clean one.
func (a Artifact) Name(binary string, date time.Time) string {
	name := fmt.Sprintf("%s-%s-%s-%s", binary, a.Token, date.UTC().Format("20060102"), a.ShortSHA)
	if a.Dirty {
		name += "-dirty"
	}
	return name
}

// CommitStamp renders the commit identity to inject via ldflags
// (-X main.commit): the full HEAD sha, with an explicit -dirty suffix when
// the tree is dirty. Artifact builds inject this and disable the Go
// toolchain's own VCS stamping (-buildvcs=false) because that stamping is
// untrustworthy from linked worktrees: nested under the repo dir it records
// the MAIN checkout's HEAD and dirty state (verified live — a worktree
// build at eb743642c embedded the main checkout's 50e120757), and outside
// the repo dir it records nothing.
func (a Artifact) CommitStamp() string {
	stamp := a.HeadSHA
	if a.Dirty {
		stamp += "-dirty"
	}
	return stamp
}

// BaseStamp renders the build's relationship to its base lineage at build
// time — e.g. "Voxist/main@eb743642c+0-0" — for embedding via ldflags so
// `gc version` can answer "how far from the fork's main was this build?"
// without rev-list archeology.
func (a Artifact) BaseStamp() string {
	return fmt.Sprintf("%s@%s+%d-%d", a.BaseRef, a.BaseSHA, a.Ahead, a.Behind)
}

// ShellSingleQuote wraps s in single quotes for safe eval in POSIX shells,
// escaping embedded single quotes. Used by `go run ./cmd/artifactname
// -format eval`, whose output a Makefile recipe evals.
func ShellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func sanitizeToken(s string) string {
	return strings.Trim(tokenSanitizeRe.ReplaceAllString(s, "-"), "-")
}

func gitOut(repoPath string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoPath}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
)

// BundledPackProvenanceCheck compares every served bundled-pack cache's
// recorded content hash against the RUNNING binary's embedded content hash
// (vp-fkrl). The defect this closes: a cache's only provenance used to be
// the canonical PIN constant, so a fork-built binary reported served fork
// content as gastownhall@f895c0ff, and triage reading those surfaces
// concluded fork-only fixes were structurally undeliverable when they were
// already shipped and served. A content-hash mismatch here is the
// shipped-but-not-served regression made detectable — the generic form of
// the ADR-0091 Site-1 defect.
//
// Deliberately THREE-valued (ADR-0091): served (hash matches) /
// stale-served (hash differs) / UNOBSERVED (no cache found). Unobserved is
// a Warning, never an OK — a verifier that verifies nothing reads as
// coverage (ADR-0113, gastown-pin-probe files_compared=0).
type BundledPackProvenanceCheck struct {
	// CacheRoot resolves the repo-cache root to scan. Injectable for tests.
	CacheRoot func() (string, error)
	// ExpectedHash returns this binary's embedded bundled-content hash.
	// Injectable for tests.
	ExpectedHash func() (string, error)
	// RunningRevision is this binary's build revision, reported as advisory
	// context beside each cache's recorded builder (built_by). Injectable
	// for tests; empty renders "unknown".
	RunningRevision string
}

// NewBundledPackProvenanceCheck returns the check wired to the real cache
// root and the real embedded-content hash. runningRevision is the value
// `gc version` reports as the commit.
func NewBundledPackProvenanceCheck(runningRevision string) *BundledPackProvenanceCheck {
	return &BundledPackProvenanceCheck{
		CacheRoot:       config.GlobalRepoCacheRoot,
		ExpectedHash:    builtinpacks.SyntheticContentHash,
		RunningRevision: runningRevision,
	}
}

// Name returns the check identifier.
func (c *BundledPackProvenanceCheck) Name() string { return "bundled-pack-provenance" }

// bundledCacheMarker mirrors the descriptor fields this check reads.
// Unknown fields (a cache written before built_by existed) decode as "".
type bundledCacheMarker struct {
	ContentHash string `toml:"content_hash"`
	BuiltBy     string `toml:"built_by"`
}

// Run scans the repo-cache root for served bundled-pack caches and compares
// each descriptor's content hash with this binary's embedded content.
func (c *BundledPackProvenanceCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	root, err := c.CacheRoot()
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("cannot resolve the repo cache root; bundled-pack provenance unobserved: %v", err)
		return r
	}
	expected, err := c.ExpectedHash()
	if err != nil {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("cannot hash this binary's embedded packs; bundled-pack provenance unobserved: %v", err)
		return r
	}

	dirEntries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("no repo cache root at %s; nothing served to verify (unobserved, not OK)", root)
			return r
		}
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("reading repo cache root %s: %v", root, err)
		return r
	}

	var dirs []string
	for _, e := range dirEntries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)

	revision := c.RunningRevision
	if revision == "" {
		revision = "unknown"
	}
	var served, stale, unknown int
	for _, dir := range dirs {
		path := filepath.Join(root, dir, builtinpacks.MarkerFilename)
		data, err := os.ReadFile(path)
		if err != nil {
			continue // not a bundled-pack cache directory
		}
		var marker bundledCacheMarker
		if err := toml.Unmarshal(data, &marker); err != nil {
			r.Details = append(r.Details, fmt.Sprintf("%s: undecodable marker (%v)", dir, err))
			unknown++
			continue
		}
		if marker.ContentHash == "" {
			r.Details = append(r.Details, fmt.Sprintf("%s: marker records no content_hash", dir))
			unknown++
			continue
		}
		served++
		if marker.ContentHash == expected {
			detail := fmt.Sprintf("%s: served content matches this binary (%s, built_by %s)",
				dir, marker.ContentHash, displayRevision(marker.BuiltBy))
			if marker.BuiltBy != "" && c.RunningRevision != "" &&
				!strings.HasPrefix(c.RunningRevision, marker.BuiltBy) &&
				!strings.HasPrefix(marker.BuiltBy, c.RunningRevision) {
				detail += fmt.Sprintf(" — cache materialized by a DIFFERENT build (%s) with identical content", displayRevision(marker.BuiltBy))
			}
			r.Details = append(r.Details, detail)
			continue
		}
		stale++
		r.Details = append(r.Details, fmt.Sprintf("%s: STALE — served %s but this binary embeds %s (built_by %s; this binary %s)",
			dir, marker.ContentHash, expected, displayRevision(marker.BuiltBy), revision))
	}

	if served == 0 && stale == 0 && unknown == 0 {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("no bundled-pack cache found under %s; nothing verified (unobserved, not OK)", root)
		return r
	}
	if stale > 0 || unknown > 0 {
		r.Status = StatusError
		r.Message = fmt.Sprintf("%d of %d served bundled-pack cache(s) do NOT match this binary's embedded content — shipped-but-not-served content is live on this host", stale+unknown, served+stale+unknown)
		r.FixHint = "let the next gc import rematerialize the cache, or redeploy so every gc process runs this binary"
		return r
	}
	r.Status = StatusOK
	r.Message = fmt.Sprintf("%d served bundled-pack cache(s) match this binary's embedded content (%s, built by %s)",
		served, expected, revision)
	return r
}

// displayRevision renders an empty recorded builder honestly instead of
// guessing it from the canonical pin (the vp-fkrl falsification).
func displayRevision(rev string) string {
	if strings.TrimSpace(rev) == "" {
		return "unknown"
	}
	return rev
}

// CanFix returns false — the doctor never deletes or rewrites served
// caches; rematerialization is the import path's own self-heal.
func (c *BundledPackProvenanceCheck) CanFix() bool { return false }

// Fix is a no-op; the check is not auto-fixable.
func (c *BundledPackProvenanceCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; the check observes deploy state, not
// anything `gc start` must establish.
func (c *BundledPackProvenanceCheck) WarmupEligible() bool { return false }

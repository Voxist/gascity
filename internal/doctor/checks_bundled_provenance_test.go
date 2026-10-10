package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/builtinpacks"
)

// TestBundledPackProvenanceCheck exercises the vp-fkrl guard against REAL
// marker files in a REAL temp cache root (ADR-0113 RULE 4: the real failing
// call, not a mock). Three-valued on purpose: served / stale-served /
// unobserved — and unobserved must never read as OK.
func TestBundledPackProvenanceCheck(t *testing.T) {
	const expected = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const other = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	writeCache := func(t *testing.T, root, dir, marker string) {
		t.Helper()
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		data, err := toml.Marshal(struct {
			Schema      int    `toml:"schema"`
			Repository  string `toml:"repository"`
			Commit      string `toml:"commit"`
			ContentHash string `toml:"content_hash"`
			BuiltBy     string `toml:"built_by,omitempty"`
		}{2, builtinpacks.Repository, "f895c0ff", marker, ""})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, builtinpacks.MarkerFilename), data, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	run := func(t *testing.T, root string) *CheckResult {
		t.Helper()
		check := NewBundledPackProvenanceCheck("gcbuild321zyx")
		check.CacheRoot = func() (string, error) { return root, nil }
		check.ExpectedHash = func() (string, error) { return expected, nil }
		return check.Run(&CheckContext{})
	}

	t.Run("matching_cache_is_served", func(t *testing.T) {
		root := t.TempDir()
		writeCache(t, root, "cacheA", expected)
		r := run(t, root)
		if r.Status != StatusOK {
			t.Fatalf("Status = %v, want OK (%s)", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "match") {
			t.Errorf("Message = %q, want an explicit served match", r.Message)
		}
	})

	t.Run("stale_hash_is_the_shipped_but_not_served_regression", func(t *testing.T) {
		root := t.TempDir()
		writeCache(t, root, "cacheA", other)
		r := run(t, root)
		if r.Status != StatusError {
			t.Fatalf("Status = %v, want Error — a served cache whose bytes differ from this binary is the defect", r.Status)
		}
		if !strings.Contains(r.Message, "do NOT match") {
			t.Errorf("Message = %q, want the mismatch statement", r.Message)
		}
		if r.FixHint == "" {
			t.Errorf("FixHint empty — the operator needs the rematerialize/redeploy path")
		}
	})

	t.Run("no_cache_at_all_is_unobserved_never_ok", func(t *testing.T) {
		r := run(t, t.TempDir())
		if r.Status != StatusWarning {
			t.Fatalf("Status = %v, want Warning — verifying nothing reads as coverage (ADR-0113)", r.Status)
		}
		if !strings.Contains(r.Message, "unobserved") {
			t.Errorf("Message = %q, want the unobserved framing", r.Message)
		}
	})

	t.Run("marker_without_content_hash_counts_as_unknown_not_ok", func(t *testing.T) {
		root := t.TempDir()
		writeCache(t, root, "cacheA", "")
		r := run(t, root)
		if r.Status != StatusError {
			t.Fatalf("Status = %v, want Error — a descriptor with no content hash proves nothing", r.Status)
		}
	})

	t.Run("different_builder_same_content_is_advisory_not_stale", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "cacheA")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		data, err := toml.Marshal(struct {
			Schema      int    `toml:"schema"`
			Repository  string `toml:"repository"`
			Commit      string `toml:"commit"`
			ContentHash string `toml:"content_hash"`
			BuiltBy     string `toml:"built_by,omitempty"`
		}{2, builtinpacks.Repository, "f895c0ff", expected, "olderbuild999"})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, builtinpacks.MarkerFilename), data, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		r := run(t, root)
		if r.Status != StatusOK {
			t.Fatalf("Status = %v, want OK — identical content served by a different build is not staleness", r.Status)
		}
		joined := strings.Join(r.Details, "\n")
		if !strings.Contains(joined, "DIFFERENT build") {
			t.Errorf("Details = %q, want the advisory different-builder note", joined)
		}
	})
}

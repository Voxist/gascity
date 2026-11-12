package builtinpacks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestMaterializeSyntheticRepoRecordsBuiltBy pins the vp-fkrl descriptor
// contract: the marker records the build that MATERIALIZED the cache, never
// a guess derived from the canonical pin. The pin fields stay untouched —
// they are load-bearing for validation.
func TestMaterializeSyntheticRepoRecordsBuiltBy(t *testing.T) {
	writeMarker := func(t *testing.T, dst string) syntheticMarker {
		t.Helper()
		if err := MaterializeSyntheticRepo(dst, Repository, "f895c0ff47d6ee9334ed282a416387eb5b084d24"); err != nil {
			t.Fatalf("MaterializeSyntheticRepo: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(dst, MarkerFilename))
		if err != nil {
			t.Fatalf("ReadFile(marker): %v", err)
		}
		var marker syntheticMarker
		if err := toml.Unmarshal(data, &marker); err != nil {
			t.Fatalf("Unmarshal(marker): %v", err)
		}
		return marker
	}

	t.Run("records_the_materializing_build", func(t *testing.T) {
		orig := BuiltBy
		BuiltBy = "gcbuild123abc"
		t.Cleanup(func() { BuiltBy = orig })

		marker := writeMarker(t, t.TempDir())
		if marker.BuiltBy != "gcbuild123abc" {
			t.Errorf("marker.BuiltBy = %q, want the materializing build", marker.BuiltBy)
		}
		if marker.Commit != "f895c0ff47d6ee9334ed282a416387eb5b084d24" {
			t.Errorf("marker.Commit = %q, want the canonical pin unchanged", marker.Commit)
		}
		if !strings.HasPrefix(marker.ContentHash, "sha256:") {
			t.Errorf("marker.ContentHash = %q, want a sha256: digest", marker.ContentHash)
		}
	})

	t.Run("empty_builder_stays_empty_never_backfilled_from_pin", func(t *testing.T) {
		orig := BuiltBy
		BuiltBy = ""
		t.Cleanup(func() { BuiltBy = orig })

		marker := writeMarker(t, t.TempDir())
		if marker.BuiltBy != "" {
			t.Errorf("marker.BuiltBy = %q, want empty — a pre-stamp binary must record 'unknown', not the pin (%s)", marker.BuiltBy, marker.Commit)
		}
	})
}

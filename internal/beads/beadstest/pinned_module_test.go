package beadstest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPinnedBeadsModuleDirRefusesAnUnresolvedCache is the fence under the drift
// check's one silent-green path.
//
// The resolver is a hand-rolled copy of cmd/go's GOMODCACHE -> go env file ->
// GOPATH[0]/pkg/mod chain, kept inlined because shelling out to `go env` would
// grow the repo's shrink-only subprocess census. An inlined copy is only
// defensible if disagreeing with cmd/go is fatal, so that is asserted here
// rather than left to the one caller.
func TestPinnedBeadsModuleDirRefusesAnUnresolvedCache(t *testing.T) {
	t.Run("a cache that does not hold the module", func(t *testing.T) {
		if _, err := pinnedBeadsModuleDir(filepath.Join(t.TempDir(), "empty"), "v1.3.0"); err == nil {
			t.Fatal("pinnedBeadsModuleDir accepted a cache with no pinned module in it; a skip here lets a resolver bug read as green")
		}
	})

	t.Run("a path that is not a directory", func(t *testing.T) {
		cache := t.TempDir()
		path := filepath.Join(cache, filepath.FromSlash(PinnedBeadsModulePath)+"@v1.3.0")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not a module"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := pinnedBeadsModuleDir(cache, "v1.3.0"); err == nil {
			t.Fatal("pinnedBeadsModuleDir accepted a file where the module source should be")
		}
	})

	t.Run("the unpacked module", func(t *testing.T) {
		cache := t.TempDir()
		want := filepath.Join(cache, filepath.FromSlash(PinnedBeadsModulePath)+"@v1.3.0")
		if err := os.MkdirAll(want, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := pinnedBeadsModuleDir(cache, "v1.3.0")
		if err != nil {
			t.Fatalf("pinnedBeadsModuleDir: %v", err)
		}
		if got != want {
			t.Fatalf("pinnedBeadsModuleDir = %q, want %q", got, want)
		}
	})
}

// TestPinnedBeadsModuleDirFailsRatherThanSkips pins the seam itself.
//
// The subtests above prove the resolver returns an error; this proves what the
// caller does with it. A revert of the t.Fatalf to a t.Skipf would leave every
// other test in this package green while the pinned-cursor drift check silently
// stopped running — which is the failure mode it was written against, and it was
// reproduced with nothing more than GOMODCACHE pointed somewhere else.
func TestPinnedBeadsModuleDirFailsRatherThanSkips(t *testing.T) {
	reporter := &recordingModuleDirReporter{}
	if dir := pinnedBeadsModuleDirOrFatal(reporter, filepath.Join(t.TempDir(), "empty"), "v1.3.0"); dir != "" {
		t.Fatalf("an unresolved cache produced the directory %q", dir)
	}
	if len(reporter.skips) != 0 {
		t.Fatalf("an unresolved module cache was SKIPPED (%q); a skip lets the drift check go quiet and read as green", reporter.skips)
	}
	if len(reporter.fatals) != 1 {
		t.Fatalf("an unresolved module cache reported %d fatal(s), want exactly 1: %q", len(reporter.fatals), reporter.fatals)
	}
	for _, want := range []string{"GOMODCACHE", PinnedBeadsModulePath} {
		if !strings.Contains(reporter.fatals[0], want) {
			t.Errorf("the failure message does not name %q, so the operator cannot act on it:\n%s", want, reporter.fatals[0])
		}
	}
}

// recordingModuleDirReporter records what the resolver reports instead of
// failing or skipping the test that drives it. It does not call runtime.Goexit
// on Fatalf, so the caller returns normally and the zero value it hands back is
// asserted too.
type recordingModuleDirReporter struct {
	fatals []string
	skips  []string
}

func (r *recordingModuleDirReporter) Helper() {}

func (r *recordingModuleDirReporter) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recordingModuleDirReporter) Skipf(format string, args ...any) {
	r.skips = append(r.skips, fmt.Sprintf(format, args...))
}

// TestLinkedBeadsModuleFollowsAReplace pins the bridge case: with go.mod
// replacing github.com/steveyegge/beads, the go command links (and the module
// cache holds) the replacement, case-escaped, so the drift check must read
// that directory rather than fail on the requirement's never-filled one.
func TestLinkedBeadsModuleFollowsAReplace(t *testing.T) {
	cases := []struct {
		name, goMod, wantPath, wantVersion string
		wantReplaced, wantErr              bool
	}{
		{name: "no replace", goMod: "module x\n\nrequire github.com/steveyegge/beads v1.3.0\n"},
		{
			name:         "single line",
			goMod:        "require github.com/steveyegge/beads v1.3.0\nreplace github.com/steveyegge/beads => github.com/Voxist/beads v1.1.1-0.20261113233511-b0548453ae8d // bridge\n",
			wantPath:     "github.com/Voxist/beads",
			wantVersion:  "v1.1.1-0.20261113233511-b0548453ae8d",
			wantReplaced: true,
		},
		{
			name:         "block",
			goMod:        "replace (\n\tgithub.com/other/mod => github.com/other/fork v1.0.0\n\tgithub.com/steveyegge/beads v1.3.0 => github.com/Voxist/beads v1.2.3\n)\n",
			wantPath:     "github.com/Voxist/beads",
			wantVersion:  "v1.2.3",
			wantReplaced: true,
		},
		{name: "another module only", goMod: "replace github.com/other/mod => github.com/other/fork v1.0.0\n"},
		{name: "local path", goMod: "replace github.com/steveyegge/beads => ../beads\n", wantErr: true},
	}
	for _, tc := range cases {
		path, version, replaced, err := beadsReplacement(tc.goMod)
		if (err != nil) != tc.wantErr || replaced != tc.wantReplaced || path != tc.wantPath || version != tc.wantVersion {
			t.Errorf("%s: beadsReplacement = (%q, %q, %v, %v), want (%q, %q, %v, err=%v)",
				tc.name, path, version, replaced, err, tc.wantPath, tc.wantVersion, tc.wantReplaced, tc.wantErr)
		}
	}

	if got := escapeModulePath("github.com/Voxist/beads"); got != "github.com/!voxist/beads" {
		t.Errorf("escapeModulePath = %q, want github.com/!voxist/beads", got)
	}

	cache := t.TempDir()
	want := filepath.Join(cache, "github.com", "!voxist", "beads@v1.2.3")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}
	reporter := &recordingModuleDirReporter{}
	if got := linkedModuleDirOrFatal(reporter, cache, "github.com/Voxist/beads", "v1.2.3"); got != want {
		t.Errorf("linkedModuleDirOrFatal = %q, want %q (fatals: %v)", got, want, reporter.fatals)
	}
	reporter = &recordingModuleDirReporter{}
	linkedModuleDirOrFatal(reporter, cache, "github.com/Voxist/beads", "v9.9.9")
	if len(reporter.fatals) != 1 || len(reporter.skips) != 0 {
		t.Errorf("a replaced module missing from the cache must be fatal, never a skip: fatals=%q skips=%q", reporter.fatals, reporter.skips)
	}
}

package beads

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestOpenNativeDoltStorageRaisesPoolReadTimeoutToReadBudget pins the per-I/O
// read deadline a native open hands the library. Left at the library's 10s
// default, a read slower than 10s on a loaded server is cut client-side,
// classified transient, and re-issued inside the 90s read budget while the
// server keeps executing every abandoned copy (ga-s69mk.7: up to ~8 executions
// of one listing per read). The open therefore defaults the deadline to the
// read budget, and leaves an operator's explicit choice — env or the
// workspace config.yaml — to the library's own precedence.
func TestOpenNativeDoltStorageRaisesPoolReadTimeoutToReadBudget(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		config string
		want   string
	}{
		{name: "unset defaults to read budget", want: nativeReadRetryBudget.String()},
		{name: "explicit env wins", env: "15s", want: "15s"},
		{name: "nested config key left to library", config: "dolt:\n  pool-read-timeout: 20s\n", want: ""},
		{name: "flat config key left to library", config: "dolt.pool-read-timeout: 20s\n", want: ""},
		{name: "unrelated config defaults to read budget", config: "dolt:\n  disable-event-flush: true\n", want: nativeReadRetryBudget.String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BD_EVENTS_JOURNAL", "")
			t.Setenv(nativePoolReadTimeoutEnvKey, tc.env)
			if tc.env == "" {
				if err := os.Unsetenv(nativePoolReadTimeoutEnvKey); err != nil {
					t.Fatal(err)
				}
			}
			beadsDir := t.TempDir()
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var seen string
			oldOpen := nativeDoltOpenBestAvailable
			t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
			nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
				seen = os.Getenv(nativePoolReadTimeoutEnvKey)
				return &nativeDoltStorageSpy{}, nil
			}

			if _, err := openNativeDoltStorage(context.Background(), beadsDir); err != nil {
				t.Fatalf("openNativeDoltStorage: %v", err)
			}
			if seen != tc.want {
				t.Fatalf("library saw %s=%q during open, want %q", nativePoolReadTimeoutEnvKey, seen, tc.want)
			}
			after, present := os.LookupEnv(nativePoolReadTimeoutEnvKey)
			if tc.env == "" && present {
				t.Fatalf("%s=%q leaked past the open window", nativePoolReadTimeoutEnvKey, after)
			}
			if tc.env != "" && after != tc.env {
				t.Fatalf("%s = %q after open, want the operator's %q restored", nativePoolReadTimeoutEnvKey, after, tc.env)
			}
		})
	}
}

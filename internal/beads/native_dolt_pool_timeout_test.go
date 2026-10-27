package beads

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestOpenNativeDoltStorageDefaultsPoolReadTimeout pins the per-I/O read
// deadline a native open hands the library. Left at the library's 10s
// default, a read slower than 10s on a loaded server is cut client-side,
// classified transient, and re-issued inside the 90s read budget while the
// server keeps executing every abandoned copy (ga-s69mk.7: up to ~8 executions
// of one listing per read). The open therefore defaults the deadline to
// nativePoolReadTimeoutDefault, and leaves an operator's choice — env or the
// workspace config.yaml — to the library's own precedence, but only when the
// library would actually use that value: an empty, zero, negative or
// unparseable setting makes the library fall back to its 10s default, so gc's
// default must still apply.
func TestOpenNativeDoltStorageDefaultsPoolReadTimeout(t *testing.T) {
	gcDefault := nativePoolReadTimeoutDefault.String()
	cases := []struct {
		name   string
		envSet bool
		env    string
		config string
		want   string
	}{
		{name: "unset defaults", want: gcDefault},
		{name: "explicit env duration wins", envSet: true, env: "15s", want: "15s"},
		{name: "explicit env bare seconds wins", envSet: true, env: "45", want: "45"},
		{name: "empty env defaults", envSet: true, env: "", want: gcDefault},
		{name: "blank env defaults", envSet: true, env: "  ", want: gcDefault},
		{name: "zero env defaults", envSet: true, env: "0", want: gcDefault},
		{name: "negative env defaults", envSet: true, env: "-5s", want: gcDefault},
		{name: "unparseable env defaults", envSet: true, env: "soon", want: gcDefault},
		{name: "nested config duration left to library", config: "dolt:\n  pool-read-timeout: 20s\n", want: ""},
		{name: "flat config duration left to library", config: "dolt.pool-read-timeout: 20s\n", want: ""},
		{name: "config bare integer seconds left to library", config: "dolt:\n  pool-read-timeout: 30\n", want: ""},
		{name: "zero config defaults", config: "dolt:\n  pool-read-timeout: 0\n", want: gcDefault},
		{name: "unparseable config defaults", config: "dolt.pool-read-timeout: soon\n", want: gcDefault},
		{name: "empty config value defaults", config: "dolt.pool-read-timeout: \"\"\n", want: gcDefault},
		{name: "unparseable config file defaults", config: "dolt: [unclosed\n", want: gcDefault},
		{name: "unusable env falls through to usable config", envSet: true, env: "soon", config: "dolt.pool-read-timeout: 20s\n", want: "soon"},
		{name: "unrelated config defaults", config: "dolt:\n  disable-event-flush: true\n", want: gcDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BD_EVENTS_JOURNAL", "")
			t.Setenv(nativePoolReadTimeoutEnvKey, tc.env)
			if !tc.envSet {
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
			if present != tc.envSet || after != tc.env {
				t.Fatalf("%s after open = (%q, present=%v), want the original (%q, present=%v) restored",
					nativePoolReadTimeoutEnvKey, after, present, tc.env, tc.envSet)
			}
		})
	}
}

// TestNativePoolReadTimeoutDefaultLeavesRoomForOneReconnect pins the sizing
// rule: a read on a silently dead peer must be able to time out, reconnect and
// retry once inside the read budget.
func TestNativePoolReadTimeoutDefaultLeavesRoomForOneReconnect(t *testing.T) {
	if nativePoolReadTimeoutDefault >= nativeReadRetryBudget {
		t.Fatalf("nativePoolReadTimeoutDefault = %s, want below nativeReadRetryBudget %s", nativePoolReadTimeoutDefault, nativeReadRetryBudget)
	}
}

// TestOpenNativeDoltStorageRestoresPoolReadTimeoutOnPanic pins that a panic
// inside the library open cannot leave gc's default in the process env, where
// every later open would read it as the operator's choice.
func TestOpenNativeDoltStorageRestoresPoolReadTimeoutOnPanic(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "")
	t.Setenv(nativePoolReadTimeoutEnvKey, "")
	if err := os.Unsetenv(nativePoolReadTimeoutEnvKey); err != nil {
		t.Fatal(err)
	}
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		panic("library open panicked")
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("openNativeDoltStorage did not propagate the library panic")
			}
		}()
		_, _ = openNativeDoltStorage(context.Background(), t.TempDir())
	}()
	if value, present := os.LookupEnv(nativePoolReadTimeoutEnvKey); present {
		t.Fatalf("%s=%q leaked into the process env after a panicking open", nativePoolReadTimeoutEnvKey, value)
	}
}

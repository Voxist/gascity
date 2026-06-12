package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// portFileFixture builds a managed bd-store city with one rig and returns
// the paths plus a config naming the rig.
func portFileFixture(t *testing.T) (cityDir, rigDir string, cfg *config.City) {
	t.Helper()
	cityDir = t.TempDir()
	rigDir = filepath.Join(t.TempDir(), "alpha")
	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg = &config.City{
		Workspace: config.Workspace{Name: "portfile-test"},
		Rigs:      []config.Rig{{Name: "alpha", Path: rigDir, Prefix: "al"}},
	}
	return cityDir, rigDir, cfg
}

func livePortFixed() func(string) (liveDoltPortResolution, error) {
	return func(string) (liveDoltPortResolution, error) {
		return liveDoltPortResolution{Port: 28231, Source: liveDoltHandleSource}, nil
	}
}

func livePortUnavailable() func(string) (liveDoltPortResolution, error) {
	return func(string) (liveDoltPortResolution, error) {
		return liveDoltPortResolution{}, errors.New("no live managed dolt endpoint")
	}
}

func writeScopeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".beads", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runPortFileCheck(t *testing.T, cityDir string, cfg *config.City, livePort func(string) (liveDoltPortResolution, error)) *doctor.CheckResult {
	t.Helper()
	check := newPortFileConsistencyCheck(cityDir, cfg)
	check.livePort = livePort
	return check.Run(&doctor.CheckContext{})
}

func TestPortFileConsistency_MatchingFilesOK(t *testing.T) {
	cityDir, rigDir, cfg := portFileFixture(t)
	writeScopeFile(t, cityDir, "dolt-server.port", "28231\n")
	writeScopeFile(t, rigDir, "dolt-server.port", "28231\n")

	r := runPortFileCheck(t, cityDir, cfg, livePortFixed())
	if r.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK; message=%q details=%v", r.Status, r.Message, r.Details)
	}
}

func TestPortFileConsistency_CityPortFileMismatchIsError(t *testing.T) {
	cityDir, _, cfg := portFileFixture(t)
	writeScopeFile(t, cityDir, "dolt-server.port", "29000\n")

	r := runPortFileCheck(t, cityDir, cfg, livePortFixed())
	if r.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want Error; message=%q", r.Status, r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "29000") || !strings.Contains(joined, "28231") {
		t.Errorf("Details missing both ports: %v", r.Details)
	}
	if !strings.Contains(joined, "dolt-server.port") {
		t.Errorf("Details missing file name: %v", r.Details)
	}
}

func TestPortFileConsistency_RigPortFileMismatchIsError(t *testing.T) {
	cityDir, rigDir, cfg := portFileFixture(t)
	writeScopeFile(t, cityDir, "dolt-server.port", "28231\n")
	writeScopeFile(t, rigDir, "dolt-server.port", "29000\n")

	r := runPortFileCheck(t, cityDir, cfg, livePortFixed())
	if r.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want Error; message=%q", r.Status, r.Message)
	}
	if joined := strings.Join(r.Details, "\n"); !strings.Contains(joined, "alpha") {
		t.Errorf("Details missing rig scope attribution: %v", r.Details)
	}
}

func TestPortFileConsistency_UnparseablePortFileIsError(t *testing.T) {
	cityDir, _, cfg := portFileFixture(t)
	writeScopeFile(t, cityDir, "dolt-server.port", "not-a-port\n")

	r := runPortFileCheck(t, cityDir, cfg, livePortFixed())
	if r.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want Error for unparseable port file", r.Status)
	}
}

func TestPortFileConsistency_ProxiedClientInfoMismatchIsError(t *testing.T) {
	cityDir, _, cfg := portFileFixture(t)
	writeScopeFile(t, cityDir, "proxied_server_client_info.json", `{"external":{"host":"127.0.0.1","port":29000,"user":"root"}}`)

	r := runPortFileCheck(t, cityDir, cfg, livePortFixed())
	if r.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want Error; message=%q", r.Status, r.Message)
	}
	if joined := strings.Join(r.Details, "\n"); !strings.Contains(joined, "proxied_server_client_info.json") {
		t.Errorf("Details missing client-info file name: %v", r.Details)
	}
}

func TestPortFileConsistency_ProxiedClientInfoMatchingOK(t *testing.T) {
	cityDir, _, cfg := portFileFixture(t)
	writeScopeFile(t, cityDir, "proxied_server_client_info.json", `{"external":{"host":"127.0.0.1","port":28231,"user":"root"}}`)

	r := runPortFileCheck(t, cityDir, cfg, livePortFixed())
	if r.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK; message=%q details=%v", r.Status, r.Message, r.Details)
	}
}

func TestPortFileConsistency_NoLiveListenerWithFilesWarns(t *testing.T) {
	cityDir, _, cfg := portFileFixture(t)
	writeScopeFile(t, cityDir, "dolt-server.port", "28231\n")

	r := runPortFileCheck(t, cityDir, cfg, livePortUnavailable())
	if r.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning when status files exist but no live listener resolves", r.Status)
	}
}

func TestPortFileConsistency_NoLiveListenerNoFilesOK(t *testing.T) {
	cityDir, _, cfg := portFileFixture(t)

	r := runPortFileCheck(t, cityDir, cfg, livePortUnavailable())
	if r.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK; message=%q", r.Status, r.Message)
	}
}

func TestPortFileConsistency_NotBdStoreWorkspaceOK(t *testing.T) {
	cityDir := t.TempDir() // no .beads/metadata.json anywhere
	cfg := &config.City{Workspace: config.Workspace{Name: "plain"}}

	r := runPortFileCheck(t, cityDir, cfg, livePortFixed())
	if r.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK for non-bd workspace", r.Status)
	}
}

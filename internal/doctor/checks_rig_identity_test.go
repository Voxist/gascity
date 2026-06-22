package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// RED tests for RigIdentityTriadCheck (vp-cz7o.12). They fail to compile until
// the check lands in internal/doctor/checks_rig_identity.go.

func writeIdentityToml(t *testing.T, scopeRoot, id string) {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("# .beads/identity.toml — canonical, git-tracked.\n# Edited only at scope creation or by deliberate human/`gc` migration.\n\n[project]\nid = %q\n", id)
	if err := os.WriteFile(filepath.Join(beadsDir, "identity.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeMetadataProjectID(t *testing.T, scopeRoot, id string) {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var body string
	if id == "" {
		body = `{"backend":"dolt","database":"dolt","dolt_database":"test","dolt_mode":"server"}` + "\n"
	} else {
		body = fmt.Sprintf(`{"backend":"dolt","database":"dolt","dolt_database":"test","dolt_mode":"server","project_id":%q}`+"\n", id)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRigIdentityTriadCheck_Name(t *testing.T) {
	c := NewRigIdentityTriadCheck("", config.Rig{Name: "my-rig", Path: "/tmp"})
	if got, want := c.Name(), "rig:my-rig:identity-triad"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

func TestRigIdentityTriadCheck_WarmupEligible(t *testing.T) {
	c := NewRigIdentityTriadCheck("", config.Rig{Name: "r", Path: "/tmp"})
	if !c.WarmupEligible() {
		t.Error("WarmupEligible() = false, want true — check must fire during gc start warmup")
	}
}

func TestRigIdentityTriadCheck_CanFix(t *testing.T) {
	c := NewRigIdentityTriadCheck("", config.Rig{Name: "r", Path: "/tmp"})
	if c.CanFix() {
		t.Error("CanFix() = true, want false — identity reconciliation requires operator judgment")
	}
}

func TestRigIdentityTriadCheck_BothMatch_OK(t *testing.T) {
	rigDir := t.TempDir()
	const id = "9c64822a-baff-4b5d-b6ad-2b243ab5745a"
	writeIdentityToml(t, rigDir, id)
	writeMetadataProjectID(t, rigDir, id)

	c := NewRigIdentityTriadCheck("", config.Rig{Name: "testrip", Path: rigDir})
	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, id) {
		t.Errorf("message = %q, want project_id in message", r.Message)
	}
}

func TestRigIdentityTriadCheck_Mismatch_Error(t *testing.T) {
	rigDir := t.TempDir()
	writeIdentityToml(t, rigDir, "id-from-identity-toml")
	writeMetadataProjectID(t, rigDir, "id-from-metadata-json")

	c := NewRigIdentityTriadCheck("", config.Rig{Name: "testrip", Path: rigDir})
	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Fatalf("severity = %d, want SeverityBlocking (zero value)", r.Severity)
	}
	if !strings.Contains(r.Message, "identity mismatch") {
		t.Errorf("message = %q, want 'identity mismatch'", r.Message)
	}
	if !strings.Contains(r.Message, "id-from-identity-toml") {
		t.Errorf("message = %q, want L1 id in message", r.Message)
	}
	if !strings.Contains(r.Message, "id-from-metadata-json") {
		t.Errorf("message = %q, want L2 id in message", r.Message)
	}
	if r.FixHint == "" {
		t.Error("FixHint is empty, want a remediation hint")
	}
	if len(r.Details) == 0 {
		t.Error("Details is empty, want file paths in details")
	}
}

func TestRigIdentityTriadCheck_NeitherPresent_OK(t *testing.T) {
	rigDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := NewRigIdentityTriadCheck("", config.Rig{Name: "testrip", Path: rigDir})
	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK for legacy rig with no identity configured", r.Status, r.Message)
	}
}

func TestRigIdentityTriadCheck_NoBEADSDir_OK(t *testing.T) {
	rigDir := t.TempDir()
	// .beads/ directory doesn't exist at all

	c := NewRigIdentityTriadCheck("", config.Rig{Name: "testrip", Path: rigDir})
	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK when .beads/ absent", r.Status, r.Message)
	}
}

func TestRigIdentityTriadCheck_L1AbsentL2Present_Warning(t *testing.T) {
	rigDir := t.TempDir()
	writeMetadataProjectID(t, rigDir, "some-project-id")
	// no identity.toml

	c := NewRigIdentityTriadCheck("", config.Rig{Name: "testrip", Path: rigDir})
	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning when identity.toml absent but metadata.json has project_id", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("severity = %d, want SeverityAdvisory", r.Severity)
	}
}

func TestRigIdentityTriadCheck_L1PresentL2Absent_Warning(t *testing.T) {
	rigDir := t.TempDir()
	writeIdentityToml(t, rigDir, "some-project-id")
	writeMetadataProjectID(t, rigDir, "") // metadata.json without project_id

	c := NewRigIdentityTriadCheck("", config.Rig{Name: "testrip", Path: rigDir})
	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning when L1 present but L2 project_id absent", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("severity = %d, want SeverityAdvisory", r.Severity)
	}
}

func TestRigIdentityTriadCheck_IdentityTomlParseError_Warning(t *testing.T) {
	rigDir := t.TempDir()
	beadsDir := filepath.Join(rigDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// malformed TOML
	if err := os.WriteFile(filepath.Join(beadsDir, "identity.toml"), []byte("[project\nid = \"bad\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewRigIdentityTriadCheck("", config.Rig{Name: "testrip", Path: rigDir})
	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning on parse error", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("severity = %d, want SeverityAdvisory on parse error", r.Severity)
	}
}

func TestRigIdentityTriadCheck_MetadataJSONParseError_Warning(t *testing.T) {
	rigDir := t.TempDir()
	const id = "9c64822a-baff-4b5d-b6ad-2b243ab5745a"
	writeIdentityToml(t, rigDir, id)
	beadsDir := filepath.Join(rigDir, ".beads")
	// malformed JSON
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewRigIdentityTriadCheck("", config.Rig{Name: "testrip", Path: rigDir})
	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning on metadata.json parse error", r.Status, r.Message)
	}
}

func TestRigIdentityTriadCheck_RelativeRigPath_OK(t *testing.T) {
	cityDir := t.TempDir()
	rigSubdir := "my-rig"
	rigPath := filepath.Join(cityDir, rigSubdir)
	const id = "9c64822a-baff-4b5d-b6ad-2b243ab5745a"
	writeIdentityToml(t, rigPath, id)
	writeMetadataProjectID(t, rigPath, id)

	c := NewRigIdentityTriadCheck(cityDir, config.Rig{Name: "my-rig", Path: rigSubdir})
	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK with relative rig path", r.Status, r.Message)
	}
}

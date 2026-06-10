package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCustomTypesPreflightCheck_NoBeadsDir(t *testing.T) {
	dir := t.TempDir()
	c := NewCustomTypesPreflightCheck(dir, "test")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (no .beads dir)", r.Status)
	}
}

func mkBeadsDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestCustomTypesPreflightCheck_AllRegistered(t *testing.T) {
	dir := t.TempDir()
	mkBeadsDir(t, dir)
	c := NewCustomTypesPreflightCheck(dir, "test")
	c.ReadRegistered = func(string) ([]string, error) {
		// Every required type plus an extra registered type.
		return append(append([]string{}, RequiredCustomTypes...), "extra"), nil
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; message=%q", r.Status, r.Message)
	}
}

func TestCustomTypesPreflightCheck_MissingRequiredIsError(t *testing.T) {
	dir := t.TempDir()
	mkBeadsDir(t, dir)
	c := NewCustomTypesPreflightCheck(dir, "test")
	// Drop "session" and "convergence" from the registered set.
	c.ReadRegistered = func(string) ([]string, error) {
		var registered []string
		for _, typ := range RequiredCustomTypes {
			if typ == "session" || typ == "convergence" {
				continue
			}
			registered = append(registered, typ)
		}
		return registered, nil
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error", r.Status)
	}
	for _, want := range []string{"session", "convergence"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message %q does not name missing type %q", r.Message, want)
		}
	}
	if !strings.Contains(r.Message, "refused") {
		t.Errorf("message %q should state the flip is refused", r.Message)
	}
}

func TestCustomTypesPreflightCheck_UnreadableStoreIsWarning(t *testing.T) {
	dir := t.TempDir()
	mkBeadsDir(t, dir)
	c := NewCustomTypesPreflightCheck(dir, "test")
	c.ReadRegistered = func(string) ([]string, error) {
		return nil, errors.New("bd unreachable")
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning when store is unreadable", r.Status)
	}
}

func TestCustomTypesPreflightCheck_ReadOnly(t *testing.T) {
	c := NewCustomTypesPreflightCheck(t.TempDir(), "test")
	if c.CanFix() {
		t.Error("CanFix should be false — preflight is read-only")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix should be a no-op, got %v", err)
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible should be false")
	}
}

func TestBdTypesJSONParsing(t *testing.T) {
	out := []byte(`{"core_types":[{"name":"task","description":"x"}],"custom_types":["session","step"]}`)
	registered, err := parseBdTypesJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(registered) != 2 || registered[0] != "session" || registered[1] != "step" {
		t.Fatalf("registered = %v, want [session step]", registered)
	}
}

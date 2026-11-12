package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

// TestImportStatusBundledSourceRendersBuiltin pins the vp-fkrl text
// contract: a bundled source renders as `builtin:<pack> (gc <build>)` with
// `content:sha256:...` in the commit column — never as a fetched tree URL,
// and never with the canonical pin presented as the served bytes.
func TestImportStatusBundledSourceRendersBuiltin(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.bd]
source = "`+builtinpacks.Repository+`//examples/bd"
version = "sha:f895c0ff47d6ee9334ed282a416387eb5b084d24"
`)
	// The canonical pin sits in packs.lock, exactly as production writes it.
	fetched := time.Now()
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			builtinpacks.Repository + "//examples/bd": {
				Version: "sha:f895c0ff47d6ee9334ed282a416387eb5b084d24",
				Commit:  "f895c0ff47d6ee9334ed282a416387eb5b084d24",
				Fetched: fetched,
			},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	orig := commit
	commit = "gcbuild456def"
	t.Cleanup(func() { commit = orig })

	var stdout, stderr bytes.Buffer
	if code := doImportStatus(dir, false, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var line string
	for _, l := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(l, "pack:bd\t") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("no pack:bd row in output:\n%s", stdout.String())
	}
	cols := strings.Split(line, "\t")
	if len(cols) != 6 {
		t.Fatalf("row has %d columns, want 6: %q", len(cols), line)
	}
	if cols[1] != "builtin:bd (gc gcbuild456def)" {
		t.Errorf("source column = %q, want the builtin rendering naming the serving build", cols[1])
	}
	if cols[3] != "builtin" {
		t.Errorf("kind column = %q, want \"builtin\"", cols[3])
	}
	if !strings.HasPrefix(cols[5], "content:sha256:") {
		t.Errorf("commit column = %q, want content:<hash> — what this binary actually serves", cols[5])
	}
	if strings.Contains(cols[5], "f895c0ff") {
		t.Errorf("commit column = %q — the canonical PIN leaked into the served-content slot (the vp-fkrl falsification)", cols[5])
	}
	// The pin stays visible, factually, in the version column.
	if cols[4] != "sha:f895c0ff47d6ee9334ed282a416387eb5b084d24" {
		t.Errorf("version column = %q, want the declared pin (factual)", cols[4])
	}
}

// TestImportStatusBundledSourceJSON pins the machine-readable surface: kind
// flips to "builtin" and the builtin object names the serving build and the
// served content hash, while the authored source and lock pin stay intact
// for drift checkers that join on them.
func TestImportStatusBundledSourceJSON(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.bd]
source = "`+builtinpacks.Repository+`//examples/bd"
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			builtinpacks.Repository + "//examples/bd": {
				Version: "sha:f895c0ff47d6ee9334ed282a416387eb5b084d24",
				Commit:  "f895c0ff47d6ee9334ed282a416387eb5b084d24",
			},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	orig := commit
	commit = "gcbuild789ghi"
	t.Cleanup(func() { commit = orig })

	var stdout, stderr bytes.Buffer
	if code := doImportStatus(dir, true, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var doc ImportStatusJSON
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, stdout.String())
	}
	var entry *ImportStatusEntry
	for i := range doc.Imports {
		if doc.Imports[i].Name == "pack:bd" {
			entry = &doc.Imports[i]
		}
	}
	if entry == nil {
		t.Fatalf("no pack:bd import in document:\n%s", stdout.String())
	}
	if entry.Kind != "builtin" {
		t.Errorf("Kind = %q, want \"builtin\"", entry.Kind)
	}
	if entry.Builtin == nil {
		t.Fatalf("Builtin is nil — machine consumers cannot tell served-from-embedded")
	}
	if entry.Builtin.Pack != "bd" || entry.Builtin.BuiltBy != "gcbuild789ghi" {
		t.Errorf("Builtin = %+v, want pack=bd built_by=gcbuild789ghi", entry.Builtin)
	}
	if !strings.HasPrefix(entry.Builtin.ContentHash, "sha256:") {
		t.Errorf("ContentHash = %q, want sha256: digest of the embedded content", entry.Builtin.ContentHash)
	}
	if entry.Source != builtinpacks.Repository+"//examples/bd" {
		t.Errorf("Source = %q, want the authored source unchanged (drift checkers join on it)", entry.Source)
	}
	if entry.Pin == nil || entry.Pin.Commit != "f895c0ff47d6ee9334ed282a416387eb5b084d24" {
		t.Errorf("Pin = %+v, want the lock pin preserved (factual)", entry.Pin)
	}
}

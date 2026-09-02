// Static Makefile-text contracts for the lint targets: os.ReadFile only, no
// subprocess, so they belong in the fast push gate (ga-4h8bu split rule:
// cheap guards stay fast; the pipeline tests that drive real make/lint runs
// live in the integration-tagged lint_readonly_contract_test.go).

package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLintUsesReadonlyModuleDownloads(t *testing.T) {
	configPath := filepath.Join(repoRoot(t), ".golangci.yml")
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", configPath, err)
	}

	var config struct {
		Run struct {
			ModulesDownloadMode string `yaml:"modules-download-mode"`
		} `yaml:"run"`
	}
	if err := yaml.Unmarshal(body, &config); err != nil {
		t.Fatalf("parse %s: %v", configPath, err)
	}
	if config.Run.ModulesDownloadMode != "readonly" {
		t.Fatalf("run.modules-download-mode = %q, want readonly", config.Run.ModulesDownloadMode)
	}

	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	const readonlyGOFlags = "QUALITY_GATE_GOFLAGS = $$(go env GOFLAGS | sed -E 's/(^|[[:space:]])-mod=[^[:space:]]+//g') -mod=readonly"
	if !strings.Contains(string(makefile), readonlyGOFlags) {
		t.Fatalf("Makefile must derive QUALITY_GATE_GOFLAGS from effective GOFLAGS")
	}
	for target, wantGOFLAGS := range map[string]string{
		"lint-full":     `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"lint-new":      `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"lint-changed":  `export GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"lint-affected": `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
	} {
		t.Run(target, func(t *testing.T) {
			body := makeTargetBody(t, string(makefile), target)
			for _, override := range []string{"--config", "--no-config"} {
				if strings.Contains(body, override) {
					t.Fatalf("%s overrides shared lint configuration with %q", target, override)
				}
			}
			if strings.Contains(body, "--modules-download-mode") {
				t.Fatalf("%s must not rely on a lint CLI module-mode override", target)
			}
			if !strings.Contains(body, wantGOFLAGS) {
				t.Fatalf("%s must scope QUALITY_GATE_GOFLAGS to its subprocess tree", target)
			}
		})
	}
}

func TestQualityGateTargetsUseReadonlyModuleDownloads(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	const readonlyGOFlags = "QUALITY_GATE_GOFLAGS = $$(go env GOFLAGS | sed -E 's/(^|[[:space:]])-mod=[^[:space:]]+//g') -mod=readonly"
	if !strings.Contains(string(makefile), readonlyGOFlags) {
		t.Fatalf("Makefile must normalize QUALITY_GATE_GOFLAGS from effective GOFLAGS")
	}

	for target, wantGOFLAGS := range map[string]string{
		"fmt-check":                `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"fmt-check-changed":        `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"vet":                      `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"test":                     `$(TEST_ENV) GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"test-fsys-darwin-compile": `$(TEST_ENV) GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
	} {
		t.Run(target, func(t *testing.T) {
			if body := makeTargetBody(t, string(makefile), target); !strings.Contains(body, wantGOFLAGS) {
				t.Fatalf("%s must scope QUALITY_GATE_GOFLAGS to its subprocess tree", target)
			}
		})
	}
}

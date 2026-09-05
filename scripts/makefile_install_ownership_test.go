// Static Makefile-text contracts for install/deploy path ownership. Text
// reads only, no subprocess, so they belong in the fast push gate (the
// integration-tagged makefile_install_test.go drives the real make runs).
//
// The invariant they pin (vc-lwif): `make install` writes exactly one path,
// $(INSTALL_DIR)/$(BINARY). It must never write, relink, or remove a channel
// a running deployment resolves `gc` through. Two writers at one path, with
// neither recording that it ran nor warning that it clobbered the other, is
// how this machine ran a stale supervisor image for an unknown period.
// Moving a deployment onto a dev build is `make deploy-fleet` -- explicit,
// separate, and the only Makefile target that touches those channels.

package scripts_test

import (
	"regexp"
	"strings"
	"testing"
)

// quotedText matches double-quoted shell strings, so a contract check can
// look at what a recipe *runs* rather than at what it prints about running.
var quotedText = regexp.MustCompile(`"[^"]*"`)

func recipeCommands(body string) string {
	return quotedText.ReplaceAllString(body, `""`)
}

// deployChannels are the PATH entries a running Gas City deployment can
// resolve `gc` through on a developer machine. Ordered as they appear in
// GC_DEPLOY_CHANNELS.
var deployChannels = []string{
	"$(HOME)/.local/bin/$(BINARY)",
	"$(BIN_DIR)/$(BINARY)",
	"$(GC_DEPLOY_DIR)/$(BINARY)",
}

func TestMakeInstallDoesNotWriteUserLocalBin(t *testing.T) {
	body := makeTargetBody(t, readMakefile(t), "install")

	if strings.Contains(body, ".local/bin") {
		t.Fatalf("install target references .local/bin -- it must write only "+
			"$(INSTALL_DIR)/$(BINARY); use `make deploy-fleet` to move a "+
			"deployment onto a dev build (vc-lwif).\ntarget body:\n%s", body)
	}
	for _, verb := range []string{"ln -s", "ln -sf", "ln -sfn"} {
		if strings.Contains(body, verb) {
			t.Fatalf("install target creates a symlink (%q) -- installing must "+
				"not point any other path at the build (vc-lwif).\ntarget body:\n%s", verb, body)
		}
	}
	if !strings.Contains(body, `mv -f "$$tmp" "$(INSTALL_DIR)/$(BINARY)"`) {
		t.Fatalf("install target no longer places the binary at $(INSTALL_DIR)/$(BINARY):\n%s", body)
	}
}

// TestNoMakeTargetButDeployFleetTouchesDeployChannels keeps the invariant from
// being re-broken somewhere other than `install`: deploy-fleet is the single
// Makefile writer of those paths.
func TestNoMakeTargetButDeployFleetTouchesDeployChannels(t *testing.T) {
	makefile := readMakefile(t)
	deployBody := makeTargetBody(t, makefile, "deploy-fleet")

	for _, line := range strings.Split(makefile, "\n") {
		// $(BIN_DIR) is omitted: it is $(INSTALL_DIR)'s default, so `install`
		// writing it is the point. The other two are never install's to touch.
		if !strings.Contains(line, ".local/bin") && !strings.Contains(line, ".gc/bin") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "#"):
			continue // documentation, not a recipe
		case strings.HasPrefix(trimmed, "GC_DEPLOY_"):
			continue // the channel list's own declaration
		case strings.Contains(deployBody, line):
			continue // deploy-fleet owns these paths
		default:
			t.Fatalf("Makefile line writes a deploy channel outside deploy-fleet: %q", line)
		}
	}
}

func TestDeployFleetTargetIsDeclaredAndDocumented(t *testing.T) {
	makefile := readMakefile(t)

	if !strings.Contains(makefile, "\n.PHONY: deploy-fleet\n") {
		t.Fatal("deploy-fleet must be declared .PHONY")
	}
	// `make help` greps lines starting with "## ", so the target is only
	// discoverable if it carries one. Folklore is what this replaces.
	if !strings.Contains(makefile, "\n## deploy-fleet:") {
		t.Fatal("deploy-fleet needs a `## deploy-fleet: ...` help line so `make help` lists it")
	}

	body := makeTargetBody(t, makefile, "deploy-fleet")
	for _, channel := range deployChannels {
		if !strings.Contains(makefile, channel) {
			t.Errorf("GC_DEPLOY_CHANNELS must cover %s -- all three shadow each "+
				"other on PATH, and repointing only some re-splits them (ADR-0027)", channel)
		}
	}
	if !strings.Contains(body, "$(GC_DEPLOY_CHANNELS)") {
		t.Errorf("deploy-fleet must repoint $(GC_DEPLOY_CHANNELS):\n%s", body)
	}
	// cp strips the codesign on macOS and yields a binary that dies with
	// SIGKILL 137. deploy-fleet links; it never copies.
	if strings.Contains(recipeCommands(body), "cp ") {
		t.Errorf("deploy-fleet must not copy the binary -- cp strips the macOS "+
			"codesign (SIGKILL 137); link to the built artifact instead:\n%s", body)
	}
	// -h is load-bearing: chflags follows symlinks by default, so without it
	// the restored flag lands on the artifact rather than on the channel.
	if !strings.Contains(body, "chflags -h nouchg") || !strings.Contains(body, "chflags -h uchg") {
		t.Errorf("deploy-fleet must clear and restore the uchg immutable flag "+
			"with chflags -h, rather than let a write fail silently:\n%s", body)
	}
	if !strings.Contains(body, "version") {
		t.Errorf("deploy-fleet must prove the binary runs before repointing "+
			"anything at it:\n%s", body)
	}
	// The keep-the-build-itself guard must compare file identity. A textual
	// compare recognises only the default spelling, and every other spelling
	// of the same file ends in `ln -sfn x x` -- which destroys the binary and
	// still satisfies the readlink check, so it fails silently and open.
	if !strings.Contains(body, `[ "$$channel" -ef "$$target" ]`) {
		t.Errorf("deploy-fleet must decide `keep the build itself` by file "+
			"identity (-ef), not by comparing path text:\n%s", body)
	}
}

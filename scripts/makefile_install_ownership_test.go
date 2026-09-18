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
	// $(INSTALL_DIR)/$(BINARY) is itself a deploy channel. On a deployed host
	// it is a symlink at an artifact elsewhere, and overwriting it moves every
	// context resolving gc through it. A warning cannot help -- it prints only
	// after the channel has moved -- so install must refuse.
	// Command syntax, not the substring the escape-hatch echo also contains.
	if !strings.Contains(body, `[ -z "$(GC_ALLOW_CHANNEL_OVERWRITE)" ] && [ -L "$(INSTALL_DIR)/$(BINARY)" ]`) {
		t.Errorf("install must fail closed when $(INSTALL_DIR)/$(BINARY) is a "+
			"symlink pointing outside $(INSTALL_DIR), with an explicit escape "+
			"hatch:\n%s", body)
	}
	if strings.Contains(body, "Deploy channels were NOT touched") {
		t.Errorf("install writes $(INSTALL_DIR)/$(BINARY), which IS a deploy "+
			"channel -- that reassurance is false by this Makefile's own "+
			"definitions:\n%s", body)
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
	// Narrow the haystack to the assignment: a channel merely NAMED in a
	// nearby comment would otherwise satisfy this while being off the list.
	var assignment string
	for _, line := range strings.Split(makefile, "\n") {
		if strings.HasPrefix(line, "GC_DEPLOY_CHANNELS") {
			assignment = line
			break
		}
	}
	if assignment == "" {
		t.Fatal("Makefile has no GC_DEPLOY_CHANNELS assignment")
	}
	for _, channel := range deployChannels {
		if !strings.Contains(assignment, channel) {
			t.Errorf("GC_DEPLOY_CHANNELS must cover %s -- all three shadow each "+
				"other on PATH, and repointing only some re-splits them (ADR-0027).\ngot: %s",
				channel, assignment)
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
	// Against the RAW body this passed on the error MESSAGE ("... '$$target
	// version' failed ..."), so deleting the guard itself still satisfied it --
	// the weakest assertion in the file, protecting the property most likely to
	// cause an incident. Match the command, not prose about the command.
	// These match COMMAND SYNTAX in the raw body, not a substring an error
	// message can also contain, and not recipeCommands() -- which blanks
	// double-quoted text and so erases the very arguments that make these
	// commands identifiable.
	for _, want := range []struct{ snippet, why string }{
		{
			`if ! "$$target" version >/dev/null 2>&1; then`,
			"prove the ARTIFACT runs before repointing anything at it",
		},
		{
			`if ! "$$channel" version >/dev/null 2>&1; then`,
			"prove every CHANNEL runs after repointing it -- readlink only confirms what was just written",
		},
		{
			`mv -f "$$swap" "$$channel"`,
			"swap each channel atomically via rename; `ln -sfn` unlinks then creates, leaving a window where a live channel does not exist",
		},
		{
			`[ -e "$$channel" ] && [ "$$channel" -ef "$$target" ]`,
			"decide `keep the build itself` by file identity, not by comparing path text",
		},
	} {
		if !strings.Contains(body, want.snippet) {
			t.Errorf("deploy-fleet must %s.\nmissing: %s\ntarget body:\n%s", want.why, want.snippet, body)
		}
	}
}

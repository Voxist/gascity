package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// portFileConsistencyCheck flags Dolt endpoint status files that disagree
// with the live managed listener (city-scale plan P1.7, incident class 4).
//
// .beads/dolt-server.port and .beads/proxied_server_client_info.json are
// status files: bd reads them, but gc resolves endpoints exclusively from
// live state (see dolt_port_live.go) and only mirrors values into them. A
// surviving writer has clobbered the port file with a proxy's ephemeral
// port in production (vp-w7tc), so any disagreement with the live listener
// is hard-red at zero tolerated mismatches — a lying file means raw bd
// clients are about to talk to the wrong (or no) server.
type portFileConsistencyCheck struct {
	cityPath string
	cfg      *config.City
	// livePort resolves the live managed dolt port; seam for tests.
	// Defaults to newLiveDoltPortResolver().resolve.
	livePort func(cityPath string) (liveDoltPortResolution, error)
}

func newPortFileConsistencyCheck(cityPath string, cfg *config.City) *portFileConsistencyCheck {
	return &portFileConsistencyCheck{
		cityPath: cityPath,
		cfg:      cfg,
		livePort: newLiveDoltPortResolver().resolve,
	}
}

// Name implements doctor.Check.
func (c *portFileConsistencyCheck) Name() string { return "port-file-consistency" }

// portFileScope is one directory whose .beads status files get compared
// against the live listener.
type portFileScope struct {
	label string
	dir   string
}

// Run implements doctor.Check.
func (c *portFileConsistencyCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	if c.cfg == nil || !workspaceUsesManagedBdStoreContract(c.cityPath, c.cfg.Rigs) {
		r.Status = doctor.StatusOK
		r.Message = "not using bd-backed Dolt topology"
		return r
	}

	scopes := []portFileScope{{label: "city", dir: c.cityPath}}
	for i := range c.cfg.Rigs {
		rig := c.cfg.Rigs[i]
		if strings.TrimSpace(rig.Path) == "" || !rigUsesManagedBdStoreContract(c.cityPath, rig) {
			continue
		}
		scopes = append(scopes, portFileScope{label: fmt.Sprintf("rig %q", rig.Name), dir: rig.Path})
	}

	live, liveErr := c.livePort(c.cityPath)
	if liveErr != nil || !validDoltPort(live.Port) {
		files := statusFilesPresent(scopes)
		if len(files) == 0 {
			r.Status = doctor.StatusOK
			r.Message = "no live managed Dolt listener and no Dolt endpoint status files"
			return r
		}
		r.Status = doctor.StatusWarning
		plural := ""
		if len(files) != 1 {
			plural = "s"
		}
		r.Message = fmt.Sprintf("cannot verify %d Dolt endpoint status file%s: no live managed Dolt listener resolved", len(files), plural)
		r.Details = files
		r.FixHint = "status files are never trusted by gc; if the managed Dolt should be running, start the city (`gc start`), then re-run doctor to verify the mirrors"
		return r
	}

	var mismatches []string
	for _, scope := range scopes {
		mismatches = append(mismatches, scopePortFileMismatches(scope, live.Port)...)
	}
	if len(mismatches) > 0 {
		r.Status = doctor.StatusError
		plural := ""
		if len(mismatches) != 1 {
			plural = "es"
		}
		r.Message = fmt.Sprintf("%d Dolt endpoint status-file mismatch%s against the live listener (port %d via %s)", len(mismatches), plural, live.Port, live.Source)
		r.Details = mismatches
		r.FixHint = "gc never trusts these status files (they are bd compatibility mirrors); `gc start` reconciles them to the managed port — a recurring mismatch means a surviving writer is clobbering them (vp-w7tc lineage) and must be root-caused, not tolerated"
		return r
	}

	r.Status = doctor.StatusOK
	r.Message = fmt.Sprintf("Dolt endpoint status files agree with the live listener (port %d)", live.Port)
	return r
}

// CanFix implements doctor.Check.
func (c *portFileConsistencyCheck) CanFix() bool { return false }

// Fix implements doctor.Check.
func (c *portFileConsistencyCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible implements doctor.Check. The check stays out of the
// `gc start` warm-up scan: during startup the managed Dolt may not be
// serving yet and the mirrors are about to be re-synced, so a warm-up run
// would warn spuriously. It runs on demand via `gc doctor` (and is the
// intended payload for the scheduled doctor subset in city-scale plan 1.9).
func (c *portFileConsistencyCheck) WarmupEligible() bool { return false }

// statusFilesPresent lists the Dolt endpoint status files that exist under
// the given scopes.
func statusFilesPresent(scopes []portFileScope) []string {
	var out []string
	for _, scope := range scopes {
		for _, name := range []string{"dolt-server.port", proxiedServerClientInfoFile} {
			path := filepath.Join(scope.dir, ".beads", name)
			if _, err := os.Stat(path); err == nil {
				out = append(out, fmt.Sprintf("%s: %s", scope.label, path))
			}
		}
	}
	return out
}

// scopePortFileMismatches compares one scope's status files against the live
// port and returns one detail line per disagreement. Missing files are fine
// (nothing to disagree); unparseable content counts as a mismatch because a
// raw bd client reading it cannot reach the live server either.
func scopePortFileMismatches(scope portFileScope, livePort int) []string {
	var out []string
	portFile := filepath.Join(scope.dir, ".beads", "dolt-server.port")
	if data, err := os.ReadFile(portFile); err == nil {
		text := strings.TrimSpace(string(data))
		port, convErr := strconv.Atoi(text)
		switch {
		case convErr != nil || !validDoltPort(port):
			out = append(out, fmt.Sprintf("%s: %s contains %q, not a valid port (live listener is %d)", scope.label, portFile, text, livePort))
		case port != livePort:
			out = append(out, fmt.Sprintf("%s: %s says %d but the live listener is %d", scope.label, portFile, port, livePort))
		}
	}

	infoFile := filepath.Join(scope.dir, ".beads", proxiedServerClientInfoFile)
	if data, err := os.ReadFile(infoFile); err == nil {
		var info proxiedServerClientInfo
		if jsonErr := json.Unmarshal(data, &info); jsonErr != nil {
			out = append(out, fmt.Sprintf("%s: %s is unparseable: %v (live listener is %d)", scope.label, infoFile, jsonErr, livePort))
		} else if info.External != nil && info.External.Port > 0 && info.External.Port != livePort {
			out = append(out, fmt.Sprintf("%s: %s external endpoint says %d but the live listener is %d", scope.label, infoFile, info.External.Port, livePort))
		}
	}
	return out
}

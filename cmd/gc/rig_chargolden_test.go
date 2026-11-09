package main

import (
	"testing"
)

// TestRigList_CharacterizationGolden freezes the current per-lane behavior of
// `gc rig list` across the three routing lanes. It is the second command on the
// generalized harness and the first Phase-1 migration candidate.
//
// LANE CONVERGENCE (C6, see PROGRESS.md), REVISED by vp-zdp6e: the C6
// convergence was bought with a probe, and the probe was the defect. C6 derived
// HQ Running on the API lanes from a controller probe, re-asked on every
// --json listing. That is a fixed cost paid to re-establish something the
// successful ListRigs read already proved: the server that answered IS the
// controller.
//
// HQ Running on the remote and alive lanes is therefore derived from the API
// read itself, while doRigList's serverless lane reads rigListHQRunning, a ping
// on the city's controller socket. In production the lanes stay converged,
// because the API lane is only taken when the controller is up, and a
// standalone controller and the supervisor (for each city it runs) both serve
// that socket, so the serverless lane's ping returns true as well. In THIS
// harness they diverge on purpose: the API lanes are served by an httptest
// controller (running=true) while the serverless lane has no controller at all
// (running=false). The goldens freeze that — a lane difference that reflects a
// real difference in what each lane is talking to, not a difference in how the
// value is computed. remote and alive still match each other (A==B).
//
// The city has no rigs, isolating the HQ-entry divergence and avoiding the
// tmux/session probe path (rigListSessionProvider is only built when rigs exist).
// The harness redacts the temp cityPath and resets the per-process builtin-import
// warning cache so this config-reading command is deterministic and lane-fair.
func TestRigList_CharacterizationGolden(t *testing.T) {
	h := newCharCity(t, charCityBasic, nil)
	h.runCharGolden(t, charCommand{
		name:  "rig-list",
		route: routeRigList,
		// rig list derives its data from config, not the bead store — no
		// store read-back applies.
	})
}

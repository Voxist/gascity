package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// L2's durable surface for the vc-ny00 store-warming plan.
//
// WHY A FILE AND NOT ONLY A LOG LINE. The heartbeat gauge
// (caching_store_reconcile.go) is what an operator greps; this file is what
// a PROGRAM reads. The pack-side consumer — bin/order-liveness-probe and the
// order-liveness-sentinel cooldown order, in Voxist/voxist-platform — runs
// as a separate launchd process on a 300s cadence and must decide, without
// parsing supervisor.log, whether a stale order set is explained by a
// warming store (degraded-info) or is a genuine P1 page. The same argument
// the delivery window's own record rests on applies verbatim (vp-5mc4p:
// grep -c boot-drain supervisor.log = 0 on a live 7.1MB log — production
// captures stderr nowhere durable).
//
// THIS FILE'S SCHEMA IS AN ACCEPTED INTERFACE. It is published in the PR
// description and built against by the consumer bead before this code
// ships. Add fields; never rename or repurpose one. The wire names are
// pinned by explicit json tags on beads.StoreWarmingState and by
// TestStoreWarmingStateFileSchemaIsStable.
//
// Conventions are taken wholesale from dolt-delivery-window-outcome.json
// (dolt_delivery_window.go): atomic temp-file+rename via internal/fsys, ONE
// object overwritten per transition rather than an append-only log, and an
// At timestamp that is what disambiguates a stale record from a fresh one.
// A consumer judging freshness uses each store's own LastProbeAt, not the
// file's mtime — a file rewritten for store A must not make store B's
// record look fresh.

// storeWarmingStateFileName is the durable per-store warming record's
// basename, written under the dolt pack's state dir alongside the delivery
// window's outcome record.
const storeWarmingStateFileName = "dolt-store-warming-state.json"

// storeWarmingStateRecord is the file's top-level object.
type storeWarmingStateRecord struct {
	// At is when this file was last rewritten. Present for parity with the
	// delivery-window record and for staleness triage of the file itself;
	// per-store freshness lives on each entry's LastProbeAt.
	At time.Time `json:"at"`
	// Stores carries one entry per tracked store, sorted by store id, so
	// one store warming never masks another's health.
	Stores []beads.StoreWarmingState `json:"stores"`
}

// storeWarmingStatePath resolves the durable record's path from a pack state
// dir, so it honors the same GC_PACK_STATE_DIR / GC_CITY_RUNTIME_DIR
// overrides as the rest of the managed-dolt runtime files.
func storeWarmingStatePath(packStateDir string) string {
	return filepath.Join(packStateDir, storeWarmingStateFileName)
}

// writeStoreWarmingStateFile persists the per-store warming record. Mirrors
// writeDeliveryWindowOutcomeFile exactly.
func writeStoreWarmingStateFile(packStateDir string, record storeWarmingStateRecord) error {
	path := storeWarmingStatePath(packStateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, data, 0o644)
}

// storeWarmingWriteMu serializes rewrites of the shared record. Every
// reconcile goroutine publishes through the one sink, and each publish
// rewrites the WHOLE file from the registry snapshot; without this two
// concurrent transitions could interleave their temp-file renames and the
// surviving file would be the older snapshot.
var storeWarmingWriteMu sync.Mutex

// storeWarmingStateWriteErrLogged keeps a failing write to one line per
// process. The record is best-effort observability, never a precondition
// (constraint 3) — but a write that fails silently would leave the consumer
// reading a stale file while believing it fresh, so the failure is said out
// loud once.
var storeWarmingStateWriteErrLogged sync.Once

// registerStoreWarmingStateSink points the beads-layer warming state machine
// at this city's pack state dir. Called once at supervisor start.
//
// Only the supervisor registers a sink. Every CLI invocation still runs the
// state machine and still announces on its own log line; it simply has
// nowhere durable to publish, which is the pre-plan behavior for those
// processes anyway — and is what keeps a short-lived `gc show` from
// overwriting the supervisor's record with a one-store view.
func registerStoreWarmingStateSink(cityPath string, stderr io.Writer) {
	if cityPath == "" {
		return
	}
	packStateDir := citylayout.PackStateDir(cityPath, "dolt")
	beads.SetStoreWarmingStateSink(func(beads.StoreWarmingState) {
		storeWarmingWriteMu.Lock()
		defer storeWarmingWriteMu.Unlock()
		// Rewrite from the registry rather than from the single state the
		// sink was handed: the file's contract is EVERY tracked store, and
		// a transition on one store must not drop the others.
		record := storeWarmingStateRecord{
			At:     time.Now().UTC(),
			Stores: beads.StoreWarmingStates(),
		}
		if err := writeStoreWarmingStateFile(packStateDir, record); err != nil {
			storeWarmingStateWriteErrLogged.Do(func() {
				if stderr != nil {
					fmt.Fprintf(stderr, "gc: failed to persist store warming state record: %v\n", err) //nolint:errcheck // best-effort stderr
				}
			})
		}
	})
}

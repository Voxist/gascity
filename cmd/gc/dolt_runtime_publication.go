package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func providerManagedDoltStatePath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json")
}

func readDoltRuntimeStateFile(path string) (doltRuntimeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return doltRuntimeState{}, err
	}
	var state doltRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return doltRuntimeState{}, err
	}
	return state, nil
}

// writeDoltRuntimeStateFile persists state to path atomically.
//
// It PRESERVES an existing StartedAt when the PID is unchanged. started_at
// dates a PROCESS, so while a pid keeps running its start time is a fact about
// the past and cannot legitimately move. Callers that rebuild the state from
// scratch -- `gc dolt state` defaults an omitted --started-at to time.Now()
// (cmd_dolt_state.go) -- would otherwise restamp it on every refresh, which is
// exactly what was observed on the live city: pid 83926 constant since
// 10:52:11Z while started_at walked 12:48:13 -> 12:52:02 -> 12:53:54 ->
// 13:01:53 with no restart in between (vc-8mfq).
//
// Guarding here rather than at the caller closes it for every caller, including
// ones outside this repo -- the caller driving that restamp was never
// identified. A real restart gets a new pid and so is never preserved; a stop
// write carries PID 0 and is likewise unaffected. The residual case is OS pid
// reuse, where a genuinely new process inherits the old start time; that is
// rarer, and strictly less wrong, than discarding the true start time on every
// single write.
func writeDoltRuntimeStateFile(path string, state doltRuntimeState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	state.StartedAt = preservedDoltStartedAt(path, state)
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, data, 0o644)
}

// preservedDoltStartedAt returns the StartedAt that should be written: the one
// already on disk when it names the SAME pid, otherwise the incoming one.
// Unreadable or absent prior state, a zero/changed pid, or a prior stamp that
// is empty or not RFC3339 all fall through to the incoming value, so this can
// only ever keep a well-formed start time already recorded for that pid.
//
// The pid is compared, not probed for liveness: a liveness check would not
// narrow this further, since pid reuse -- the one case where a matching pid is
// a different process -- means the reusing process IS alive. Reuse is bounded
// instead by the caller, which only ever writes a pid it just observed.
func preservedDoltStartedAt(path string, state doltRuntimeState) string {
	if state.PID <= 0 {
		return state.StartedAt
	}
	prior, err := readDoltRuntimeStateFile(path)
	if err != nil || prior.PID != state.PID {
		return state.StartedAt
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(prior.StartedAt)); err != nil {
		return state.StartedAt
	}
	return prior.StartedAt
}

func removeDoltRuntimeStateFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readPublishedDoltRuntimeStateHint(cityPath string) (doltRuntimeState, bool, error) {
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		return doltRuntimeState{}, false, fmt.Errorf("determine managed dolt ownership for published hint: %w", err)
	}
	if !owned {
		return doltRuntimeState{}, false, nil
	}
	hint, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err == nil {
		return hint, true, nil
	}
	if os.IsNotExist(err) {
		return doltRuntimeState{}, false, nil
	}
	return doltRuntimeState{}, false, fmt.Errorf("read published dolt runtime state hint: %w", err)
}

func managedDoltLifecycleOwned(cityPath string) (bool, error) {
	if cityUsesBdStoreContract(cityPath) {
		if cityUsesDoltliteBeadsBackend(cityPath) {
			return false, nil
		}
		completeBinding, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(cityPath))
		if err != nil {
			return false, err
		}
		if completeBinding {
			return false, nil
		}
		// A city whose metadata gc cannot read is not a city gc owns a Dolt
		// runtime for, and the refusal has to reach the operator rather than
		// being answered as "not owned".
		if _, _, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(cityPath)); err != nil {
			return false, err
		}
		_, _, ok, invalid := resolveConfiguredCityDoltTarget(cityPath)
		if invalid {
			return false, fmt.Errorf("invalid canonical city endpoint state")
		}
		return !ok, nil
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("load city config for managed dolt ownership: %w", err)
	}
	if cfg == nil {
		return false, nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	cityState, err := syncDesiredCityDoltConfigState(cityPath, cfg.Dolt, config.EffectiveHQPrefix(cfg))
	if err != nil {
		return false, err
	}
	if cityState.EndpointOrigin != contract.EndpointOriginManagedCity {
		return false, nil
	}
	for _, rig := range cfg.Rigs {
		if !rigUsesManagedBdStoreContract(cityPath, rig) {
			continue
		}
		rigState, err := syncDesiredRigDoltConfigState(cityPath, rig, cityState)
		if err != nil {
			return false, err
		}
		if rigState.EndpointOrigin == contract.EndpointOriginInheritedCity {
			return true, nil
		}
	}
	return false, nil
}

func syncManagedDoltPortMirrors(cityPath string) error {
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		removeDoltPortFile(cityPath)
		return nil
	}
	emitLoadCityConfigWarnings(io.Discard, prov)
	return syncConfiguredDoltPortFiles(cityPath, cfg.Dolt, config.EffectiveHQPrefix(cfg), cfg.Rigs, io.Discard)
}

func publishManagedDoltRuntimeState(cityPath string) error {
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	providerStatePath := providerManagedDoltStatePath(cityPath)
	state, readErr := readDoltRuntimeStateFile(providerStatePath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("read provider dolt runtime state: %w", readErr)
	}
	publishedHintFound := false
	if readErr != nil || !validDoltRuntimeState(state, cityPath) {
		// Provider state is missing or stale. Attempt recovery by inspecting
		// the actual running dolt process. This handles the case where dolt
		// was restarted (new PID) but the provider state file was not yet
		// updated, or where a crash left the provider state file absent.
		layout, layoutErr := resolveManagedDoltRuntimeLayout(cityPath)
		if layoutErr != nil {
			return fmt.Errorf("resolve managed dolt runtime layout: %w", layoutErr)
		}
		repaired, ok := repairedManagedDoltRuntimeState(cityPath, layout, state)
		if !ok {
			// The repair path needs a port hint. When the provider state is
			// missing, or exists but points at a dead/stale port, the published
			// runtime state is the only managed-local hint source.
			hint, found, hintErr := readPublishedDoltRuntimeStateHint(cityPath)
			if hintErr != nil {
				return hintErr
			}
			if found {
				state = hint
				publishedHintFound = true
				repaired, ok = repairedManagedDoltRuntimeState(cityPath, layout, state)
			}
		}
		if !ok {
			if readErr != nil {
				if !publishedHintFound {
					return fmt.Errorf("recover missing provider dolt runtime state: no published dolt runtime state hint")
				}
				return fmt.Errorf("recover missing provider dolt runtime state: no live managed dolt found for published port hint %d", state.Port)
			}
			return fmt.Errorf("invalid managed dolt runtime state")
		}
		// Repair the provider state file so future calls see a consistent view.
		if err := writeDoltRuntimeStateFile(providerStatePath, repaired); err != nil {
			return fmt.Errorf("repair provider dolt runtime state: %w", err)
		}
		state = repaired
	}

	return publishManagedDoltRuntimeStateFromState(cityPath, state)
}

func publishManagedDoltRuntimeStateFromState(cityPath string, state doltRuntimeState) error {
	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), state); err != nil {
		return fmt.Errorf("write published dolt runtime state: %w", err)
	}
	if err := syncManagedDoltPortMirrors(cityPath); err != nil {
		return fmt.Errorf("sync managed dolt port mirrors: %w", err)
	}
	return nil
}

func clearManagedDoltRuntimeState(cityPath string) error {
	if err := removeDoltRuntimeStateFile(managedDoltStatePath(cityPath)); err != nil {
		return fmt.Errorf("remove published dolt runtime state: %w", err)
	}
	if err := syncManagedDoltPortMirrors(cityPath); err != nil {
		return fmt.Errorf("sync managed dolt port mirrors: %w", err)
	}
	return nil
}

// clearManagedDoltRuntimeStateUnlessBound clears the published managed-Dolt
// runtime state, except for a city bound to a storage binding gc does not
// serve: that city has no managed Dolt runtime to describe, and the published
// state is not gc's to clear.
func clearManagedDoltRuntimeStateUnlessBound(cityPath string) error {
	if cityUsesBdStoreContract(cityPath) {
		completeBinding, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(cityPath))
		if err != nil {
			return err
		}
		if completeBinding {
			return nil
		}
	}
	return clearManagedDoltRuntimeState(cityPath)
}

func publishManagedDoltRuntimeStateIfOwned(cityPath string) error {
	_, err := publishManagedDoltRuntimeStateIfOwnedResult(cityPath)
	return err
}

func publishManagedDoltRuntimeStateIfOwnedResult(cityPath string) (bool, error) {
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		return false, err
	}
	if !owned {
		return false, nil
	}
	if err := publishManagedDoltRuntimeState(cityPath); err != nil {
		return false, err
	}
	return true, nil
}

func publishManagedDoltRuntimeStateIfOwnedResultFromState(cityPath string, state doltRuntimeState) (bool, error) {
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		return false, err
	}
	if !owned {
		return false, nil
	}
	if err := publishManagedDoltRuntimeStateFromState(cityPath, state); err != nil {
		return false, err
	}
	return true, nil
}

func clearManagedDoltRuntimeStateIfOwned(cityPath string) error {
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return clearManagedDoltRuntimeState(cityPath)
}

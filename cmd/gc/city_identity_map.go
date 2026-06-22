package main

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// cityTomlForIdentity is a minimal parse target for [identity_map] + [[rigs]].
// Unknown fields are ignored by the TOML decoder.
type cityTomlForIdentity struct {
	IdentityMap map[string]cityIdentityMapEntry `toml:"identity_map"`
	Rigs        []cityRigEntry                  `toml:"rigs"`
}

type cityIdentityMapEntry struct {
	ProjectID string `toml:"project_id"`
}

type cityRigEntry struct {
	Name string `toml:"name"`
	Path string `toml:"path"`
}

// readCityIdentityMapEntry returns the canonical project_id for the rig whose
// working-directory matches scopeRoot. It reads [identity_map] from
// <cityPath>/city.toml and resolves the rig name via [[rigs]].
//
// Returns ok=false (no error) when:
//   - cityPath or scopeRoot is empty
//   - city.toml does not exist
//   - [identity_map] is absent or has no entry for the rig
//   - the rig cannot be resolved from [[rigs]]
func readCityIdentityMapEntry(cityPath, scopeRoot string) (projectID string, ok bool, err error) {
	if cityPath == "" || scopeRoot == "" {
		return "", false, nil
	}
	cityPath = filepath.Clean(cityPath)
	scopeRoot = filepath.Clean(scopeRoot)

	data, readErr := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return "", false, nil
		}
		return "", false, readErr
	}

	var cfg cityTomlForIdentity
	if _, decodeErr := toml.Decode(string(data), &cfg); decodeErr != nil {
		return "", false, decodeErr
	}

	if len(cfg.IdentityMap) == 0 {
		return "", false, nil
	}

	rigName := resolveRigNameFromCityTOML(cfg.Rigs, cityPath, scopeRoot)
	if rigName == "" {
		return "", false, nil
	}

	entry, exists := cfg.IdentityMap[rigName]
	if !exists || entry.ProjectID == "" {
		return "", false, nil
	}

	return entry.ProjectID, true, nil
}

// resolveRigNameFromCityTOML returns the rig name whose effective path matches
// scopeRoot. The effective path is the explicit path field when set, otherwise
// <cityPath>/<rigName>.
func resolveRigNameFromCityTOML(rigs []cityRigEntry, cityPath, scopeRoot string) string {
	for _, rig := range rigs {
		if rig.Name == "" {
			continue
		}
		effective := filepath.Join(cityPath, rig.Name)
		if rig.Path != "" {
			effective = rig.Path
		}
		if filepath.Clean(effective) == scopeRoot {
			return rig.Name
		}
	}
	return ""
}

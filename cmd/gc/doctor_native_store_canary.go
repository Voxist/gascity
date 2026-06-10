package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// nativeStoreCanaryIdentityCheck verifies the P2.3 native-store canary lever is
// coherent: every canaried scope must carry a configured identity (project_id in
// .beads/metadata.json and issue_prefix in .beads/config.yaml) so the post-open
// identity assertion can confirm the opened database is the scope's real data.
// A canaried scope whose configured identity is missing would make every open
// degrade to configured-empty and fall back to BdStore — the lever would be
// silently inert. This check surfaces that before the cutover.
//
// It is advisory, not blocking: it never opens a Dolt server (so it stays
// dependency-free and fast); the live opened-vs-configured compare happens at
// scope-open via openNativeStoreWithIdentityAssertion.
type nativeStoreCanaryIdentityCheck struct {
	cityPath string
	cfg      *config.City
}

// newNativeStoreCanaryIdentityCheck constructs the canary identity doctor check.
func newNativeStoreCanaryIdentityCheck(cityPath string, cfg *config.City) *nativeStoreCanaryIdentityCheck {
	return &nativeStoreCanaryIdentityCheck{cityPath: cityPath, cfg: cfg}
}

func (c *nativeStoreCanaryIdentityCheck) Name() string { return "native-store-canary-identity" }

func (c *nativeStoreCanaryIdentityCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = doctor.StatusOK
		r.Message = "native-store canary off (no config)"
		return r
	}
	scopes := c.cfg.Beads.NativeStoreCanaryScopeSet(os.Getenv(config.NativeStoreCanaryEnvVar))
	if len(scopes) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "native-store canary off (no scopes)"
		return r
	}

	names := make([]string, 0, len(scopes))
	for name := range scopes {
		names = append(names, name)
	}
	sort.Strings(names)

	var incomplete []string
	for _, name := range names {
		scopeRoot, ok := c.scopeRootForName(name)
		if !ok {
			incomplete = append(incomplete, fmt.Sprintf("%s: no scope root resolved", name))
			continue
		}
		id := configuredScopeIdentity(scopeRoot)
		var missing []string
		if strings.TrimSpace(id.ProjectID) == "" {
			missing = append(missing, "project_id")
		}
		if strings.TrimSpace(id.IssuePrefix) == "" {
			missing = append(missing, "issue_prefix")
		}
		if len(missing) > 0 {
			incomplete = append(incomplete, fmt.Sprintf("%s: missing %s", name, strings.Join(missing, ", ")))
		}
	}

	if len(incomplete) == 0 {
		r.Status = doctor.StatusOK
		r.Message = fmt.Sprintf("native-store canary on for %s; all scopes carry a configured identity", strings.Join(names, ", "))
		return r
	}
	r.Status = doctor.StatusError
	r.Severity = doctor.SeverityAdvisory
	r.Message = "native-store canary enabled for scopes missing a configured identity; the post-open assertion will fall back to BdStore"
	r.Details = incomplete
	r.FixHint = "seed .beads/metadata.json project_id and .beads/config.yaml issue_prefix for each canaried scope, or remove it from native_store_canary_scopes"
	return r
}

// scopeRootForName resolves a canary scope name to its scope root: the city
// directory for the city's own name, otherwise a rig's path.
func (c *nativeStoreCanaryIdentityCheck) scopeRootForName(name string) (string, bool) {
	if c.cfg != nil {
		if strings.EqualFold(strings.TrimSpace(c.cfg.Workspace.Name), name) ||
			strings.EqualFold(strings.TrimSpace(c.cfg.ResolvedWorkspaceName), name) {
			return c.cityPath, true
		}
		for i := range c.cfg.Rigs {
			rig := c.cfg.Rigs[i]
			if !strings.EqualFold(strings.TrimSpace(rig.Name), name) {
				continue
			}
			path := rig.Path
			if path == "" {
				return "", false
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(c.cityPath, path)
			}
			return path, true
		}
	}
	return "", false
}

func (c *nativeStoreCanaryIdentityCheck) CanFix() bool { return false }

func (c *nativeStoreCanaryIdentityCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *nativeStoreCanaryIdentityCheck) WarmupEligible() bool { return false }

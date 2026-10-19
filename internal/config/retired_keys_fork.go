package config

// Fork-owned retired keys. Registering them here, rather than in the
// retiredKeys literal in undecoded.go, keeps that upstream file unchanged.

// retiredProxiedOptInAnchor is the removal anchor for the fork's [beads]
// proxied opt-in, superseded by upstream's bd-owned proxied-server.
const retiredProxiedOptInAnchor = "the Voxist fork convergence (ga-s69mk.3)"

// retiredStoreHealthKey retires the [storehealth] table and each of its leaf
// keys; the patrol it configured only ran for [beads] proxied cities.
var retiredStoreHealthKey = retiredKey{
	RemovedIn: retiredProxiedOptInAnchor,
	Note:      "the store-health patrol ran only with the retired [beads] proxied opt-in; delete the [storehealth] table",
}

func init() {
	for key, rk := range map[string]retiredKey{
		"beads.proxied": {
			RemovedIn: retiredProxiedOptInAnchor,
			Note:      "bd owns the proxied-server topology in scope metadata; migrate with `gc beads city migrate-proxied` and tune idle shutdown with [beads] proxied_idle_timeout",
		},
		"beads.proxy_pool_size": {
			RemovedIn: retiredProxiedOptInAnchor,
			Note:      "bd owns the proxied-server connection pool; migrate with `gc beads city migrate-proxied` and tune idle shutdown with [beads] proxied_idle_timeout",
		},
		"beads.proxy_idle_timeout": {
			RemovedIn: retiredProxiedOptInAnchor,
			Note:      "use [beads] proxied_idle_timeout on a city migrated with `gc beads city migrate-proxied`",
		},
		"storehealth":                      retiredStoreHealthKey,
		"storehealth.enabled":              retiredStoreHealthKey,
		"storehealth.interval":             retiredStoreHealthKey,
		"storehealth.consecutive_fails":    retiredStoreHealthKey,
		"storehealth.reap_cooldown":        retiredStoreHealthKey,
		"storehealth.write_probe_interval": retiredStoreHealthKey,
	} {
		retiredKeys[key] = rk
	}
}

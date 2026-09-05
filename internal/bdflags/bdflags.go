// Package bdflags is the single source of truth for bd CLI flag names per
// subcommand. It backs both the write-mutation ID guard in cmd/gc/cmd_bd.go
// and the gc lint check that validates bd invocations embedded in prompt
// templates, so the two call sites cannot drift apart from each other.
//
// Sourced from bd <sub> --help output (2026-07-13, bd v1.1.0); kept current
// against the pinned bd by TestBdFlagManifestCurrent (latest: bd 1.2.2 at
// 3e03250ee, 2026-09-01).
package bdflags

import (
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/gastownhall/gascity/internal/beads"
)

// globalValueFlags are accepted by every bd subcommand and consume the next
// argument as their value.
//
// Like globalBoolFlags below, this is a superset allowlist over every bd build
// the fleet and CI run (deps.env: BD_PREV_VERSION v1.0.4 through BD_SOURCE_REF
// a75c226c9), not a snapshot of one binary. Classification derived from
// `rootCmd.PersistentFlags()` in beads cmd/bd/main.go — see --format on why
// `bd --help` alone is not a sufficient source.
//
// --database added 2026-08-11 against bd a75c226c9 (fork-first repin,
// ga-zzcjs): the 0062-era proxied database override, `--database string`.
// --mem-profile takes a FILE (`--mem-profile string`) and so belongs here, not
// with the bools; classifying it as a bool made the arg walker read the
// filename as the verb. Its sibling --cpu-profile is a value-less bool and
// lives in globalBoolFlags.
//
// Which side of the split a flag lands on matters as much as its presence:
// filing a VALUE flag as a bool is the same bypass wearing a different hat —
// the flag is skipped without its value and the value is read as the verb.
// Both directions are pinned, by TestGlobalValueFlagsIsComplete and
// TestGlobalBoolFlagsIsComplete.
//
// Upstream #5601 reclassified --profile into this table and added
// --server-url. Both were rejected in the 2026-09-05 resync against
// beads cmd/bd/main.go: --profile is `PersistentFlags().BoolVar` in v1.0.4
// and in v1.1.0 (upstream's own BD_VERSION pin) and is absent from 3e03250ee
// (renamed --cpu-profile), and --server-url is not a persistent flag in any
// bd across the supported range. --format, which #5601 omits, is a real
// hidden persistent String in all three.
var globalValueFlags = map[string]bool{
	"--actor": true, "--db": true, "-C": true, "--directory": true,
	"--dolt-auto-commit": true, "--mem-profile": true,
	"--database": true,
	// --format is a HIDDEN persistent StringVar ("Output format (json). Alias
	// for --json", MarkHidden'd immediately after registration) present in
	// every supported bd from v1.0.4 to a75c226c9. Being hidden, it appears in
	// NO `bd --help` output — so neither a help-sourced audit nor
	// freshness_test.go's live `--help` probe can see it, and it stayed missing
	// here while every visible persistent flag was pinned. It is still accepted
	// before the verb and still consumes the next token: omitting it made
	// SplitGlobalFlags read "json" out of `bd --format json update <id> ...`,
	// so DroppedMetadataRefusal — scoped to verb "update" — never fired and the
	// silent 1-of-N metadata write it exists to prevent went through.
	// Verified 2026-08-31 against bd 1.1.0 (a75c226c9): `bd --format json
	// ready -n 1` is accepted and emits JSON.
	"--format": true,
}

// globalBoolFlags are accepted by every bd subcommand and take no value.
//
// This table is a superset allowlist across every bd build the fleet and CI
// run, not a snapshot of one binary. Entries are safe to keep and dangerous to
// drop: freshness_test.go's TestBdFlagManifestCurrent fails closed in exactly
// one direction — a flag the live bd has that the manifest lacks — and only
// logs the reverse.
//
// --profile is the pre-rename spelling of --cpu-profile (renamed in beads after
// the v1.1.0 tag was cut). It is a real persistent bool in beads v1.0.4, v1.0.5
// and the released v1.1.0 tag CI installs, and is rejected only by the newer
// post-rename build the fleet runs. deps.env pins BD_PREV_VERSION=v1.0.4 as the
// minimum supported bd, so both spellings are live in the supported range.
// Dropping --profile because the local binary rejects it has already broken CI
// once (3289b5673, reverted by 570e38be5) — do not do it again.
//
// --no-color added 2026-08-11 against bd a75c226c9 (the fork-first repin,
// ga-zzcjs): beads gained a persistent color kill-switch after e97839a2,
// the commit this manifest previously tracked.
//
// -V/--version is deliberately ABSENT. Despite what this comment used to
// claim, it is not persistent: beads registers it as `rootCmd.Flags().BoolP`,
// so `bd show --version` fails with "unknown flag: --version" while
// `bd --version` works. It is a root-local flag, and a table whose contract is
// "accepted by every bd subcommand" is the wrong home for it. Its absence
// costs nothing: `bd --version` and `bd -V` carry no verb, so every walker
// keyed off these maps already resolves them to "no subcommand" rather than
// mis-reading a verb (pinned by TestVersionFlagNeedsNoGlobalEntry).
// -h/--help, by contrast, IS accepted by every subcommand — cobra registers a
// help flag on each one — so it belongs here even though it is likewise not on
// the root's persistent set.
var globalBoolFlags = map[string]bool{
	"--global": true, "--ignore-schema-skew": true, "--json": true,
	"--no-color": true, "--cpu-profile": true,
	"--profile": true, "-q": true, "--quiet": true, "--readonly": true,
	"--sandbox": true, "-v": true, "--verbose": true, "-h": true, "--help": true,
}

// valueFlagsBySub holds each subcommand's value-consuming flags (beyond the
// global set), keyed by subcommand: a single word ("update") or, for
// compound bd subcommands, "parent child" ("mol pour"). The key set here
// defines every subcommand this package knows about — see Known/Subcommands.
var valueFlagsBySub = map[string]map[string]bool{
	"create": {
		// --storage-class added 2026-08-11 against bd a75c226c9 (0060-era
		// wisp storage-class selector); --allow-empty-description (bool,
		// below) same provenance.
		"--storage-class": true,
		"--acceptance":    true, "--append-notes": true, "-a": true, "--assignee": true,
		"--body-file": true, "--context": true, "--defer": true, "--deps": true,
		"-d": true, "--description": true, "--design": true, "--design-file": true,
		"--due": true, "-e": true, "--estimate": true, "--event-actor": true,
		"--event-category": true, "--event-payload": true, "--event-target": true,
		"--external-ref": true, "-f": true, "--file": true, "--graph": true,
		"--id": true, "-l": true, "--labels": true, "--metadata": true,
		"--mol-type": true, "--notes": true, "--parent": true, "-p": true,
		"--priority": true, "--repo": true, "--skills": true, "--spec-id": true,
		"-s": true, "--status": true, "--title": true, "-t": true, "--type": true, "--waits-for": true,
		"--waits-for-gate": true, "--wisp-type": true,
	},
	"update": {
		// The compare-and-set guards. Pinned as well as discovered: help
		// discovery swallows its own errors (see ValueFlagsWithDiscovery ->
		// `if out, err := runBdHelpForSubcommand(sub); err == nil`), so a bd
		// that is missing, renamed or slow leaves the pinned table as the only
		// answer — and these two are the flags whose absence broke fenced
		// dispatch in every rig.
		//
		// Added 2026-08-11 against bd a75c226c9 (0062-era conditional writes);
		// --force (bool, below) same provenance.
		"--if-assignee": true, "--if-status": true,
		"--acceptance": true, "--add-label": true, "--append-notes": true,
		"-a": true, "--assignee": true, "--await-id": true, "--body-file": true,
		"--defer": true, "-d": true, "--description": true, "--design": true,
		"--design-file": true, "--due": true, "-e": true, "--estimate": true,
		"--external-ref": true, "--metadata": true, "--notes": true,
		"--parent": true, "-p": true, "--priority": true, "--remove-label": true,
		"--session": true, "--set-labels": true, "--set-metadata": true,
		"-s": true, "--status": true, "-t": true, "--type": true,
		"--title": true, "--spec-id": true, "--unset-metadata": true,
	},
	"close": {
		"-r": true, "--reason": true, "--reason-file": true, "--session": true,
	},
	"reopen": {
		"-r": true, "--reason": true,
	},
	"delete": {
		"--from-file": true,
	},
	"ready": {
		// --label-pattern/--label-regex/--max-rows added 2026-08-11 against
		// bd a75c226c9 (fork-first repin, ga-zzcjs).
		"--label-pattern": true, "--label-regex": true, "--max-rows": true,
		"-a": true, "--assignee": true, "--exclude-label": true, "--exclude-type": true,
		"--has-metadata-key": true, "-l": true, "--label": true, "--label-any": true,
		"-n": true, "--limit": true, "--metadata-field": true, "--mol": true,
		"--mol-type": true, "--offset": true, "--parent": true, "-p": true,
		"--priority": true, "-s": true, "--sort": true, "-t": true, "--type": true,
	},
	"list": {
		"-a": true, "--assignee": true, "--closed-after": true, "--closed-before": true,
		"--created-after": true, "--created-before": true, "--defer-after": true,
		"--defer-before": true, "--desc-contains": true, "--due-after": true,
		"--due-before": true, "--exclude-label": true, "--exclude-type": true,
		// --external-contains/--external-ref/--max-rows (+ bools below)
		// added 2026-08-11 against bd a75c226c9 (fork-first repin, ga-zzcjs).
		"--external-contains": true, "--external-ref": true, "--max-rows": true,
		"--format": true, "--has-metadata-key": true, "--id": true, "-l": true,
		"--label": true, "--label-any": true, "--label-pattern": true,
		"--label-regex": true, "-n": true, "--limit": true, "--metadata-field": true,
		"--mol-type": true, "--notes-contains": true, "--offset": true,
		"--parent": true, "-p": true, "--priority": true, "--priority-max": true,
		"--priority-min": true, "--sort": true, "--spec": true, "-s": true,
		"--status": true, "--title": true, "--title-contains": true, "-t": true,
		"--type": true, "--updated-after": true, "--updated-before": true,
		"--wisp-type": true,
	},
	"show": {
		"--as-of": true, "--id": true,
	},
	"mol current": {
		"--for": true, "--limit": true, "--range": true,
	},
	"mol pour": {
		"--assignee": true, "--attach": true, "--attach-type": true, "--var": true,
	},
	"mol wisp": {
		"--var": true,
	},
	"mol burn": {},
	"gate check": {
		"-l": true, "--limit": true, "-t": true, "--type": true,
	},
	"gate list": {
		"-n": true, "--limit": true,
	},
	"dep add": {
		"--blocked-by": true, "--depends-on": true, "--file": true, "-t": true, "--type": true,
	},
	"dep list": {
		"--direction": true, "-t": true, "--type": true,
	},
	"dep remove": {},
}

// boolFlagsBySub holds each subcommand's boolean (no-value) flags beyond the
// global set. Same keying convention as valueFlagsBySub.
var boolFlagsBySub = map[string]map[string]bool{
	"create": {
		"--allow-empty-description": true,
		"--dry-run":                 true, "--ephemeral": true, "--force": true, "--no-history": true,
		"--no-inherit-labels": true, "--silent": true, "--stdin": true, "--validate": true,
	},
	"update": {
		"--force":                   true,
		"--allow-empty-description": true, "--claim": true, "--ephemeral": true,
		"--history": true, "--no-history": true, "--persistent": true, "--stdin": true,
	},
	"close": {
		"--claim-next": true, "--continue": true, "-f": true, "--force": true,
		"--no-auto": true, "--suggest-next": true,
	},
	"reopen": {},
	"delete": {
		"--cascade": true, "--dry-run": true, "-f": true, "--force": true,
	},
	"ready": {
		// --brief added 2026-09-01 against bd 3e03250ee (schema-0066 repin).
		"--brief": true,
		"--claim": true, "--explain": true, "--gated": true, "--include-deferred": true,
		"--include-ephemeral": true, "--plain": true, "--pretty": true, "-u": true, "--unassigned": true,
	},
	"list": {
		// --brief added 2026-09-01 against bd 3e03250ee (schema-0066 repin).
		"--brief": true,
		"--deps":  true,
		"--all":   true, "--deferred": true, "--empty-description": true, "--flat": true,
		"--include-ephemeral": true,
		"--include-gates":     true, "--include-infra": true, "--include-templates": true,
		"--long": true, "--no-assignee": true, "--no-labels": true, "--no-pager": true,
		"--no-parent": true, "--no-pinned": true, "--overdue": true, "--pinned": true,
		"--pretty": true, "--ready": true, "-r": true, "--reverse": true,
		"--skip-labels": true, "--tree": true, "-w": true, "--watch": true,
	},
	"show": {
		// --brief-deps added 2026-09-01 against bd 3e03250ee (schema-0066 repin).
		"--brief-deps": true,
		"--children":   true, "--current": true, "--include-comments": true,
		"--include-dependents": true, "--local-time": true, "--long": true,
		"--refs": true, "--short": true, "--thread": true, "-w": true, "--watch": true,
	},
	"mol current": {},
	"mol pour": {
		"--dry-run": true,
	},
	"mol wisp": {
		"--dry-run": true, "--root-only": true,
	},
	"mol burn": {
		"--dry-run": true, "--force": true,
	},
	"gate check": {
		"--dry-run": true, "-e": true, "--escalate": true,
	},
	"gate list": {
		"-a": true, "--all": true,
	},
	"dep add": {
		"--no-cycle-check": true,
	},
	"dep list":   {},
	"dep remove": {},
}

// GlobalValueFlags returns the flags accepted by every bd subcommand that
// consume the next argument as their value. A caller locating the subcommand in
// an argv must skip these values, or it reads one of them as the verb.
func GlobalValueFlags() map[string]bool {
	return mergeFlagSets(globalValueFlags)
}

// GlobalBoolFlags returns the flags accepted by every bd subcommand that
// consume nothing. It is the companion of GlobalValueFlags for a caller that
// must also tell a KNOWN valueless flag from an unrecognized one — the
// difference between "skip this token and keep looking for the verb" and "the
// verb is no longer decidable from this argv".
func GlobalBoolFlags() map[string]bool {
	return mergeFlagSets(globalBoolFlags)
}

// Subcommands returns the bd subcommand keys this package has flag
// manifests for (e.g. "close", "mol pour"), in no particular order.
func Subcommands() []string {
	keys := make([]string, 0, len(valueFlagsBySub))
	for k := range valueFlagsBySub {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Known reports whether sub is a subcommand key this package has a flag
// manifest for.
func Known(sub string) bool {
	_, ok := valueFlagsBySub[sub]
	return ok
}

// ValueFlags returns the set of value-consuming flag names (long and short
// form) for sub, merged with the global flags shared by every bd
// subcommand. Returns nil if sub is not a known subcommand key.
//
// This is the static, pinned-table lookup: it never shells out to `bd`.
// Callers that gate whether bd gets invoked at all (the write-mutation ID
// guard in cmd_bd.go, the by-id door in cmd_bd_by_id.go, the split-city
// class-projection refusal in bd_relocated_classes.go) depend on that —
// TestGcBdListRefusesAGraphClassProjectionOnASplitCity asserts bd is never
// invoked on a refused path, and a live discovery probe here would violate
// that invariant as a side effect of a flag-name lookup. Use
// ValueFlagsWithDiscovery for text-analysis contexts (the `gc lint` bd-flag
// check) that may legitimately shell out.
func ValueFlags(sub string) map[string]bool {
	subFlags, ok := valueFlagsBySub[sub]
	if !ok {
		return nil
	}
	return mergeFlagSets(globalValueFlags, subFlags)
}

// BoolFlags returns the set of boolean flag names for sub, merged with the
// global boolean flags shared by every bd subcommand. Returns nil if sub is
// not a known subcommand key. See ValueFlags: this is the static, pinned-
// table lookup and never shells out to `bd`.
func BoolFlags(sub string) map[string]bool {
	subFlags, ok := boolFlagsBySub[sub]
	if !ok {
		return nil
	}
	return mergeFlagSets(globalBoolFlags, subFlags)
}

// ValueFlagsWithDiscovery returns ValueFlags(sub) augmented with flags
// discovered by shelling out to `bd <sub> --help`, so the flag table can
// stay current without a manual re-transcription each time bd adds a flag.
// Discovery failure (bd missing, renamed, slow) silently falls back to the
// static table, which is why the compare-and-set flags are pinned there
// too rather than relying on discovery alone.
//
// Only for contexts where invoking bd as a side effect is acceptable — the
// `gc lint` check that validates bd invocations embedded in prompt
// templates (ScanUnknownFlags) is pure text analysis, not a guard deciding
// whether to invoke bd, so a live probe there is safe.
func ValueFlagsWithDiscovery(sub string) map[string]bool {
	base := ValueFlags(sub)
	if base == nil {
		return nil
	}
	return mergeFlagSets(base, parseDiscoveredFlags(sub).value)
}

// BoolFlagsWithDiscovery is BoolFlags augmented with live discovery. See
// ValueFlagsWithDiscovery.
func BoolFlagsWithDiscovery(sub string) map[string]bool {
	base := BoolFlags(sub)
	if base == nil {
		return nil
	}
	return mergeFlagSets(base, parseDiscoveredFlags(sub).bool)
}

type discoveredFlags struct {
	value map[string]bool
	bool  map[string]bool
}

var (
	parseDiscoveredOnce sync.Map

	runBdHelpForSubcommand = func(sub string) ([]byte, error) {
		parts := strings.Split(sub, " ")
		args := append(parts, "--help")
		runner := beads.ExecCommandRunner()
		return runner(".", "bd", args...)
	}
)

func parseDiscoveredFlags(sub string) discoveredFlags {
	if cached, ok := parseDiscoveredOnce.Load(sub); ok {
		if parsed, ok := cached.(discoveredFlags); ok {
			return parsed
		}
	}

	parsed := discoveredFlags{
		value: make(map[string]bool),
		bool:  make(map[string]bool),
	}

	if out, err := runBdHelpForSubcommand(sub); err == nil {
		parseHelpFlagsToSets(string(out), &parsed)
	}

	parseDiscoveredOnce.Store(sub, parsed)
	return parsed
}

// parseHelpFlagsToSets reads cobra-style help output and classifies flags as
// value-consuming vs. boolean across both local and global flag sections.
func parseHelpFlagsToSets(helpText string, dst *discoveredFlags) {
	inFlags := false
	for _, rawLine := range strings.Split(helpText, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "Flags:"):
			inFlags = true
			continue
		case strings.HasPrefix(line, "Global Flags:"):
			inFlags = true
			continue
		case !strings.HasPrefix(line, "-"):
			inFlags = false
			continue
		}

		if !inFlags {
			continue
		}

		tokens := strings.Fields(line)
		if len(tokens) < 1 {
			continue
		}

		var (
			isValue bool
			flags   []string
		)

		for i, token := range tokens {
			flag := strings.TrimSuffix(token, ",")
			if !strings.HasPrefix(flag, "-") {
				if i > 0 && isValueToken(flag) {
					isValue = true
				}
				break
			}
			if flag != "-" && flag != "--" {
				flags = append(flags, flag)
			}
		}

		for _, flag := range flags {
			if isValue {
				dst.value[flag] = true
			} else {
				dst.bool[flag] = true
			}
		}
	}
}

func isValueToken(token string) bool {
	t := strings.Trim(token, "[]")
	t = strings.TrimSuffix(strings.TrimPrefix(t, "<"), ">")
	if t == "" {
		return false
	}

	if strings.ContainsAny(t, "\\") {
		return false
	}

	// Type annotations that are not boolean.
	switch t {
	case "string", "stringArray", "stringSlice", "duration", "int", "int64", "uint", "uint64", "float64", "float32", "time.Duration", "path", "file", "type", "id", "ids", "args", "name", "keys", "value":
		return true
	case "bool", "boolean":
		return false
	}

	if strings.Contains(t, "[") || strings.Contains(t, "]") {
		return true
	}

	if strings.ContainsRune(token, '<') && strings.ContainsRune(token, '>') {
		return true
	}

	for _, r := range t {
		if unicode.IsUpper(r) {
			return false
		}
	}

	return false
}

func mergeFlagSets(sets ...map[string]bool) map[string]bool {
	merged := make(map[string]bool)
	for _, set := range sets {
		for k := range set {
			merged[k] = true
		}
	}
	return merged
}

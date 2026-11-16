package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/spf13/cobra"
)

// newReadyCmd builds `gc ready`: the in-process, city-wide claimable-work reader
// that federates the stores a split city spreads its work across.
//
// The reader itself is internal/readywork, shared with the supervisor's
// GET /v0/city/{cityName}/work/ready; this file is its CLI projection. On a city
// that relocates nothing it reads the stores it always read.
//
// The output is a bare JSON array in bd's row shape, because it is a drop-in for
// the `bd ready --json` the default work_query builds; see readywork.ReadyBead.
func newReadyCmd(stdout, stderr io.Writer) *cobra.Command {
	var flags readyFlags
	var jsonOut bool
	var includeEphemeral bool
	cmd := &cobra.Command{
		Use:   "ready",
		Short: "List ready (claimable) work across every store in the city",
		Long: `List ready, claimable work as a JSON array, federating the city store, the
rig stores, and — on a split city — the relocated graph store where molecule
roots, step beads and control beads live.

It is the in-process, city-wide drop-in for the single-store "bd ready" work
query, so workers and the control plane see graph-class work instead of an
authoritative-looking short answer. On a city that relocates no coordination
class there is no graph store to add: it reads the city store and the rig
stores.

The flags mirror the "bd ready" contract the default work_query builds:
  gc ready --metadata-field "gc.routed_to=$target" --unassigned \
           --exclude-type=epic --exclude-label "hold:mayor" \
           --exclude-label "hold:external" --exclude-label "human" \
           --json --limit=20

Rows are emitted in canonical ready order (priority, created_at, id), which
--sort priority also selects. --sort takes the rest of bd ready's policies with
bd's semantics, hybrid (rows created in the last 48 hours by priority, then
older rows oldest first) and oldest, plus newest. --limit is applied last, so a
bounded read is the true top-N of the merged set rather than the top-N of
whichever store answered first.

A failed read fails as bd ready does: it writes nothing to stdout, names the
cause on stderr, and exits non-zero. stdout carries a JSON array and nothing
else, and --json adds no failure envelope.

The rows of --status in_progress carry their blockers (blocked_by), and a row
whose blockers cannot be resolved fails the read. --until-unblocked ends that
answer at its first unblocked row, the one a crash-recovery read resumes, and
resolves no row past it, so a later row's unresolvable blocker does not fail
the read.

Every leg is read across both storage tiers, so the wisp/ephemeral rows an
orchestration step runs as are claimable work here whether or not
--include-ephemeral is passed.

--fields narrows each row to exactly the given wire field names (repeatable
and/or comma-separated, e.g. --fields id,priority,status,title,metadata), in
canonical wire order regardless of the order given; an unknown name is
rejected before any store is read. --no-description is shorthand for --fields
with every field except description, which measured 78% of a 918KB federated
read's payload on a routed-work query (vp-7llgp) and is bytes most callers
that only need id/priority/status/metadata pay for nothing. The two flags
cannot be combined. Pass neither for the full, unchanged default row.

Suspended rigs are not read: rigs suspended with "gc rig suspend", or declared
suspended_on_start and not resumed. Their agents claim nothing and the
controller counts no demand in them, so this reads the same rigs the controller
does; their work comes back when the rig is resumed. The city store and a
relocated graph store are always read.

When the city's controller is running, it answers the query from the stores it
already holds open (GET /v0/city/{cityName}/work/ready), with the reader this
command runs, reading every store live, never from a cache. It reads the rigs
of the config the controller has loaded: a rig added, removed or rebound in the
city's config takes effect in its answer at the controller's next config
reload. It refuses while a config reload or an API config write is in
progress, and after a failed reload until one succeeds; a [storage] change
fails every reload until it is reverted or the city restarts. If the
controller is not running, refuses, or does not answer within 10s, this command
loads the config and reads the stores itself. GC_NO_API=1 forces the local
read.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdReady(flags, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	registerReadyFlags(cmd, &flags, &includeEphemeral, &jsonOut)
	return cmd
}

// readyFlags is the `gc ready` flag surface: the reader's query, which is also
// the query GET /v0/city/{cityName}/work/ready takes, and untilUnblocked, the
// cut --until-unblocked makes of an in_progress answer
// (readywork.ReadUntilUnblocked). The cut is not part of the query because the
// endpoint does not take it: on that route the command cuts the controller's
// whole answer itself.
type readyFlags struct {
	query          readywork.Query
	untilUnblocked bool
	// fields and noDescription are the --fields/--no-description projection
	// flags (vp-7llgp). They narrow presentation only: the query above still
	// decides which rows match, these decide which of a matched row's fields
	// are emitted.
	fields        []string
	noDescription bool
}

// validate checks every flag without reading anything.
func (f readyFlags) validate() error {
	if f.noDescription && len(f.fields) > 0 {
		return fmt.Errorf("--fields and --no-description cannot be combined")
	}
	for _, name := range f.fields {
		if !readyWireFieldSet[name] {
			return fmt.Errorf("unknown --fields entry %q (known fields: %s)", name, strings.Join(readyWireFieldOrder, ", "))
		}
	}
	if f.untilUnblocked {
		return readywork.ValidateUntilUnblocked(f.query)
	}
	return f.query.Validate()
}

// projectionFields resolves the --fields/--no-description flags into the
// canonical-order field list writeReadyRows projects each row onto, or nil
// when neither flag is set — nil means "no projection", the full, unchanged
// default row.
func (f readyFlags) projectionFields() []string {
	if f.noDescription {
		out := make([]string, 0, len(readyWireFieldOrder))
		for _, name := range readyWireFieldOrder {
			if name != "description" {
				out = append(out, name)
			}
		}
		return out
	}
	if len(f.fields) == 0 {
		return nil
	}
	want := make(map[string]bool, len(f.fields))
	for _, name := range f.fields {
		want[name] = true
	}
	out := make([]string, 0, len(want))
	for _, name := range readyWireFieldOrder {
		if want[name] {
			out = append(out, name)
		}
	}
	return out
}

// readyWireFieldOrder is the JSON wire order of readywork.ReadyBead's fields,
// derived by reflection so --fields/--no-description can never drift from the
// struct's real field set (see TestReadyWireFieldSetIsPinnedToTheHTTPBeadShape,
// which pins the same struct from the test side).
var readyWireFieldOrder = readyBeadJSONFieldOrder()

// readyWireFieldSet is readyWireFieldOrder as a membership set, for rejecting
// an unknown --fields entry before any store is read.
var readyWireFieldSet = func() map[string]bool {
	set := make(map[string]bool, len(readyWireFieldOrder))
	for _, name := range readyWireFieldOrder {
		set[name] = true
	}
	return set
}()

// readyBeadJSONFieldOrder reads readywork.ReadyBead's JSON tags in struct
// declaration order, which is the order json.Marshal emits them in.
func readyBeadJSONFieldOrder() []string {
	t := reflect.TypeOf(readywork.ReadyBead{})
	names := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// read answers the flags in-process over legs.
func (f readyFlags) read(legs []readywork.Leg) ([]readywork.ReadyBead, error) {
	if f.untilUnblocked {
		return readywork.ReadUntilUnblocked(context.Background(), legs, f.query)
	}
	return readywork.Read(legs, f.query)
}

// registerReadyFlags binds the `gc ready` flag surface onto cmd.
//
// It is a function rather than an inline block so the SAME flag surface can be
// parsed outside the command — specifically by the reader-agreement conformance
// test, which takes the routed-demand tier straight out of the generated work
// query, parses it here, and runs the real filterReadyBeads over a row corpus.
// A test that re-declared these flags would be testing its own copy of the
// contract, which is the drift this whole lane exists to remove.
func registerReadyFlags(cmd *cobra.Command, flags *readyFlags, includeEphemeral, jsonOut *bool) {
	opts := &flags.query
	cmd.Flags().StringVar(&opts.Assignee, "assignee", "", "only work assigned to exactly this identity")
	cmd.Flags().BoolVar(&opts.Unassigned, "unassigned", false, "only unassigned work")
	cmd.Flags().StringArrayVar(&opts.MetadataFields, "metadata-field", nil, "require metadata \"key=value\", or bare \"key\" for any non-empty value (repeatable)")
	cmd.Flags().StringArrayVar(&opts.ExcludeTypes, "exclude-type", nil, "drop beads of this issue type (repeatable)")
	cmd.Flags().StringArrayVar(&opts.ExcludeLabels, "exclude-label", nil, "drop beads carrying this label (repeatable)")
	cmd.Flags().StringVar(&opts.Sort, "sort", "", "sort order: priority|hybrid|oldest|newest (default: priority, the canonical ready order)")
	cmd.Flags().IntVar(&opts.Limit, "limit", 0, "max beads to return (0 = unlimited)")
	// --include-ephemeral is accepted for parity with `bd ready`, which the
	// generated work_query carries verbatim on a bd-1.0.5 city. It selects
	// nothing, and that is the honest spelling rather than a silent one: every
	// leg is read at beads.FederatedReadTier, which already spans the wisp tier.
	// Binding it to a discarded variable is deliberate — a readywork.Query field
	// nothing consults is how a flag ends up looking honored while one leg
	// quietly narrows (ga-8lyxc).
	cmd.Flags().BoolVar(includeEphemeral, "include-ephemeral", false, "accept --include-ephemeral for bd-ready parity (every leg already spans the wisp tier)")
	cmd.Flags().StringVar(&opts.Status, "status", "", "list beads in this status instead of ready work: open|in_progress|blocked|closed")
	cmd.Flags().BoolVar(&flags.untilUnblocked, "until-unblocked", false, "with --status in_progress, end the answer at its first unblocked row, resolving no row past it")
	cmd.Flags().StringSliceVar(&flags.fields, "fields", nil, "emit only these wire field names instead of the full row (repeatable, comma-separated); unknown names are rejected")
	cmd.Flags().BoolVar(&flags.noDescription, "no-description", false, "omit the description field alone (shorthand for --fields with every field except description); cannot combine with --fields")
	// --json is accepted for parity with `bd ready --json`, which the generated
	// work_query carries verbatim. The payload is a JSON array either way; the
	// flag exists so the query does not have to be rewritten to drop it.
	cmd.Flags().BoolVar(jsonOut, "json", true, "accept --json for bd-ready parity (output is always a JSON array)")
}

func cmdReady(flags readyFlags, stdout, stderr io.Writer) int {
	// Every flag is validated before anything is resolved, loaded or opened: a
	// malformed query must not cost a city-wide federated read to be told it was
	// malformed.
	if err := flags.validate(); err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// One composed city-config load for the whole invocation. Every store open
	// below resolves its own inputs — the pinned bd, the storage binding env —
	// and each of those composed the city config again, once per rig: 22 loads
	// for one `gc ready` on a six-rig bd-backed city. They are all answering for
	// the config this command is about to load, so the memo serves them from it
	// (see invocationConfigMemo for what callers may and may not touch).
	defer beginInvocationConfigMemo()()
	// The reader is city-wide: no rig context selects anything, so the
	// resolution skips the full config load a rig name costs.
	cityPath, err := resolveCityPathOnly()
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if rows, ok := readyThroughController(cityPath, flags.query, stderr); ok {
		if flags.untilUnblocked {
			// The controller answers the whole window with every row resolved, and
			// its cut is the in-process read's answer. A row past the cut that cannot
			// be resolved fails the controller's read instead, and the in-process
			// read below answers.
			rows = readywork.CutUntilUnblocked(rows)
		}
		if err := writeReadyRows(stdout, rows, flags.projectionFields()); err != nil {
			fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cityStore, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rigStores, err := readyRigLegStores(cfg, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	legs, err := readyFederationLegs(cityPath, loadedCityName(cfg, cityPath), cfg, cityStore, rigStores)
	if err != nil {
		// A refused city: the plan carries the refusal that names the remedy, and
		// a work-only answer here is a short array indistinguishable from "no
		// work" for every work query this reader backs.
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rows, err := flags.read(legs)
	if err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := writeReadyRows(stdout, rows, flags.projectionFields()); err != nil {
		fmt.Fprintf(stderr, "gc ready: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// writeReadyRows writes rows as the `gc ready` contract: the full wire rows
// unchanged when fields is empty (the default), or each row narrowed to
// exactly fields, in canonical wire order, when it is not.
func writeReadyRows(stdout io.Writer, rows []readywork.ReadyBead, fields []string) error {
	if len(fields) == 0 {
		return writeJSON(stdout, rows)
	}
	projected, err := projectReadyRows(rows, fields)
	if err != nil {
		return err
	}
	return writeJSON(stdout, projected)
}

// readyProjectedRow is one row narrowed to a field subset. It implements
// json.Marshaler so the emitted object's key order follows fields (the
// canonical wire order readyFlags.projectionFields already resolved it to)
// rather than Go's map iteration order.
type readyProjectedRow struct {
	fields []string
	raw    map[string]json.RawMessage
}

func (p readyProjectedRow) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for _, name := range p.fields {
		val, ok := p.raw[name]
		if !ok {
			// An omitempty field absent from the row's own marshaling (e.g. an
			// unset assignee) stays absent under projection too, rather than
			// being synthesized as null or a zero value.
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		key, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// projectReadyRows narrows each row to exactly fields, which must already be
// in canonical wire order (readyFlags.projectionFields does this).
func projectReadyRows(rows []readywork.ReadyBead, fields []string) ([]readyProjectedRow, error) {
	out := make([]readyProjectedRow, 0, len(rows))
	for _, row := range rows {
		data, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("marshal row %s: %w", row.ID, err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("decode row %s: %w", row.ID, err)
		}
		out = append(out, readyProjectedRow{fields: fields, raw: raw})
	}
	return out, nil
}

// readyAPITimeout bounds how long `gc ready` waits for the controller before it
// reads in-process instead. The controller answers a claim tier in tens to
// hundreds of milliseconds and a whole unbounded ready set (eleven thousand rows
// on a large city) in a few seconds; past this, the request is hung or the
// controller is overloaded, and the in-process read is the faster route. A
// variable so tests can stand up a hung controller without waiting this long.
var readyAPITimeout = 10 * time.Second

// readyAPIClient resolves the controller `gc ready` asks, or nil with the reason
// it reads in-process instead. supervisorFallthroughAPIClient is the routing for
// a read whose local path is expensive: a supervisor-managed city answers on the
// supervisor's port even though its per-city controller socket is alive, where
// apiClient would send it to the local path. A variable so tests can stand a
// controller up, or take it away.
var readyAPIClient = supervisorFallthroughAPIClient

// readyThroughController asks the controller holding this city's stores open
// (GET /v0/city/{cityName}/work/ready) for the answer. That answer is the
// in-process reader's over the config the controller has published and the
// stores it holds: the same rows in the same order with the same fields, read
// live (see controllerState.ReadyWork). The process then opens no store at all.
// It is not a fresh read of the config on disk: a rig added, removed or rebound
// there takes effect in the controller's answer at its next config reload, and
// in the in-process read at once.
//
// It answers false on ANY failure, and the caller reads in-process exactly as it
// always has: no controller, a controller from before the endpoint existed, a
// refused request (a reload or API config write in progress, or a failed
// reload), a store the controller could not read, a transport error or a
// timeout. That read is the one `gc ready` made before the endpoint existed, so
// falling back to it is always safe. GC_NO_API forces it.
func readyThroughController(cityPath string, q readywork.Query, stderr io.Writer) ([]readywork.ReadyBead, bool) {
	client, reason := readyAPIClient(cityPath)
	if client == nil {
		logRoute(stderr, "ready", "fallback", reason)
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), readyAPITimeout)
	defer cancel()
	rows, err := client.ReadyWork(ctx, q)
	if err != nil {
		logRoute(stderr, "ready", "fallback", "api-error", "error", strconv.Quote(err.Error()))
		return nil, false
	}
	logRoute(stderr, "ready", "api", "")
	return rows, true
}

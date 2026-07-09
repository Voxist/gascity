package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// statusResponse is the JSON body for GET /v0/status.
// TODO(huma): replace with StatusBody once migration is complete.
type statusResponse = StatusBody

type (
	agentCounts = StatusAgentCounts
	rigCounts   = StatusRigCounts
	workCounts  = StatusWorkCounts
	mailCounts  = StatusMailCounts
)

var statusStoreReadTimeout = time.Second

// statusWorkExcludedTypes are bead types counted as infrastructure, not
// work, by the status endpoint's work-count buckets.
var statusWorkExcludedTypes = []string{"message", "convoy", "convergence"}

// StatusInput is the Huma input for GET /v0/status.
type StatusInput struct {
	CityScope
	BlockingParam
	// Lite trims the body to the cheap fleet-overview fields for
	// high-frequency dashboard polls, omitting the expensive per-request
	// blocks: StoreHealth (full closed-history Dolt scan), the
	// session-count detail, and the per-rig work-count fan-out. The
	// default/full body that `gc status` renders is unchanged (gascity#3186).
	Lite bool `query:"lite" required:"false" doc:"When true, omit the expensive store-health, session-count, and work-count blocks for low-cost dashboard polls."`
}

// humaHandleStatus is the Huma-typed handler for GET /v0/status.
//
// Read-path gate: refuses to serve while the city-scope CachingStore is
// priming (cacheLiveOr503 → typed 503) so the CLI falls back to its local
// snapshot instead of rendering partial/empty data. CacheAgeS surfaces the
// age of the latest fresh observation so `gc status` can append a staleness
// banner when the supervisor is lagging.
func (s *Server) humaHandleStatus(ctx context.Context, input *StatusInput) (*IndexOutput[StatusBody], error) {
	store := s.state.CityBeadStore()
	if err := cacheLiveOr503(store); err != nil {
		return nil, err
	}
	bp := input.toBlockingParams()
	blocking := bp.isBlocking()
	if blocking {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}
	index := s.latestIndex()

	// Strict-freshness callers (blocking ?index=&wait=) asked to observe a
	// specific event, so they get a fresh synchronous build and bypass every
	// cache — the body must reflect the event they waited for.
	if blocking {
		resp := s.buildStatusBody(ctx, input.Lite)
		return &IndexOutput[StatusBody]{Index: index, CacheAgeS: cacheAgeSeconds(store), Body: resp}, nil
	}

	cacheKey := "status"
	if input.Lite {
		cacheKey = "status?lite"
	}

	// Fast path: a body built within the current time bucket (≤2s). On a busy
	// city the event sequence advances every poll, so the cache keys on a TIME
	// bucket rather than the event index (gascity#3186); high-frequency
	// dashboard polls reuse the built body. The ?lite variant caches under its
	// own key.
	bucket := responseCacheTimeBucket(time.Now())
	if body, ok := cachedResponseAs[StatusBody](s, cacheKey, bucket); ok {
		return &IndexOutput[StatusBody]{Index: index, CacheAgeS: cacheAgeSeconds(store), Body: body}, nil
	}

	// Warm StatusView (vp-e0hv): serve the last background-built body instead of
	// running the ~28s O(store-size) buildStatusBody on the request path — that
	// synchronous build is what made every cold `gc status` time out and fall
	// back to the slow local probe. Refresh in the background once the body ages
	// past statusWarmRefreshAfter; never block the request on the rebuild.
	if entry, ok := s.warmStatusBody(input.Lite); ok {
		age := time.Since(entry.builtAt)
		if age <= statusWarmServeMaxAge {
			if age > statusWarmRefreshAfter {
				s.refreshStatusBodyAsync(input.Lite)
			}
			// entry.body is served without the cloneCachedValue deep-copy used by
			// the bucket-cache path above. Safe because a built StatusBody is
			// immutable after publish: buildStatusBody constructs every slice
			// fresh and the two pointer fields (StoreHealth, Beads) are
			// replace-not-mutate, and nothing appends to a stored body. If a
			// future handler enriches the body in place before serialization,
			// clone here (matching response_cache.go's clone-on-read invariant).
			return &IndexOutput[StatusBody]{Index: index, CacheAgeS: cacheAgeSeconds(store), Body: entry.body}, nil
		}
	}

	// No usable warm body (cold start or long idle): build synchronously once,
	// single-flighted so a burst of cold requests shares one build, then serve
	// it and seed the warm entry. Only this first build after a (re)start or
	// long idle pays the O(store) cost; every subsequent request is served from
	// the warm entry above without blocking.
	resp := s.buildAndStoreStatus(input.Lite)
	return &IndexOutput[StatusBody]{Index: index, CacheAgeS: cacheAgeSeconds(store), Body: resp}, nil
}

// buildStatusBody constructs the status response body. ctx bounds the
// per-store work-count queries; cancellation aborts in-flight backend
// counts.
//
// When lite is true the expensive per-request blocks are omitted for
// high-frequency dashboard polls (gascity#3186): the work-count fan-out
// (a query per rig store), the StoreHealth block (a full closed-history
// Dolt row scan), and the session-count detail. The session snapshot itself
// is still read because agent running/suspended state depends on it. The full
// (non-lite) body that `gc status` renders is unchanged.
func (s *Server) buildStatusBody(ctx context.Context, lite bool) StatusBody {
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	sessTmpl := cfg.Workspace.SessionTemplate
	sessionSnapshot := s.statusSessionSnapshot()
	partialErrors := append([]string(nil), sessionSnapshot.partialErrors...)

	citySt, _ := suspensionstate.Load(fsys.OSFS{}, s.state.CityPath())

	// Count agents by state and collect per-agent detail rows in a single
	// pass. Pool expansion emits one detail row per instance with a
	// once-per-group ScaleLabel so the CLI's text formatter can indent the
	// expanded rows the same way it does in the fallback path.
	var ac agentCounts
	var rawRunning int
	agentDetails := make([]StatusAgentDetail, 0, len(cfg.Agents))
	suspendedRigs := make(map[string]bool, len(cfg.Rigs))
	for _, r := range cfg.Rigs {
		if suspensionstate.EffectiveRigSuspended(citySt, r.Name, r.EffectiveSuspendedOnStart()) {
			suspendedRigs[r.Name] = true
		}
	}
	perRigAgentTotals := make(map[string]int, len(cfg.Rigs))
	perRigAgentsSuspended := make(map[string]int, len(cfg.Rigs))
	for _, a := range cfg.Agents {
		rigName := workdirutil.ConfiguredRigName(s.state.CityPath(), a, cfg.Rigs)
		scope := "city"
		if rigName != "" {
			scope = "rig"
		}
		expanded := expandAgent(a, cityName, sessTmpl, sp)
		expanded = appendUnlimitedPoolSessionBeads(expanded, a, cityName, sessTmpl, sessionSnapshot)
		isPool := len(expanded) > 1 || a.SupportsInstanceExpansion()
		groupName := a.QualifiedName()
		scaleLabelEmitted := false
		for _, ea := range expanded {
			ac.Total++
			if rigName != "" {
				perRigAgentTotals[rigName]++
			}
			sessName := agentSessionName(cityName, ea.qualifiedName, sessTmpl)
			info, hasInfo := sessionSnapshot.bySessionName[sessName]
			running := statusProviderRunning(sp, sessName)
			if running {
				rawRunning++
			}
			suspended := ea.suspended || a.Suspended || (rigName != "" && suspendedRigs[rigName]) || (hasInfo && info.state == session.StateSuspended)
			if suspended && rigName != "" {
				perRigAgentsSuspended[rigName]++
			}
			switch {
			case suspended:
				ac.Suspended++
			case s.state.IsQuarantined(sessName):
				ac.Quarantined++
			case running:
				ac.Running++
			}

			detail := StatusAgentDetail{
				QualifiedName: ea.qualifiedName,
				Scope:         scope,
				Running:       running,
				Suspended:     suspended,
				SessionName:   sessName,
				GroupName:     groupName,
				Expanded:      isPool,
			}
			if isPool {
				_, instanceName := config.ParseQualifiedName(ea.qualifiedName)
				detail.Name = instanceName
				if !scaleLabelEmitted {
					detail.ScaleLabel = poolScaleLabel(a)
					scaleLabelEmitted = true
				}
			} else {
				detail.Name = a.Name
			}
			agentDetails = append(agentDetails, detail)
			if a.Dir != "" {
				perRigAgentTotals[a.Dir]++
				if suspended {
					perRigAgentsSuspended[a.Dir]++
				}
			}
		}
	}

	// Count rigs by state + collect per-rig detail rows.
	rc := rigCounts{Total: len(cfg.Rigs)}
	rigDetails := make([]StatusRigDetail, 0, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rigSuspended := suspensionstate.EffectiveRigSuspended(citySt, rig.Name, rig.EffectiveSuspendedOnStart())
		if !rigSuspended {
			if total := perRigAgentTotals[rig.Name]; total > 0 && total == perRigAgentsSuspended[rig.Name] {
				rigSuspended = true
			}
		}
		if rigSuspended {
			rc.Suspended++
			suspendedRigs[rig.Name] = true
		}
		rigDetails = append(rigDetails, StatusRigDetail{
			Name:      rig.Name,
			Path:      rig.Path,
			Suspended: rigSuspended,
		})
	}

	// Count work items (best-effort). Skipped in lite mode: querying every
	// rig store is one of the per-request costs the lite poll avoids.
	var wc workCounts
	if !lite {
		var workErrs []string
		wc, workErrs = s.statusWorkCounts(ctx)
		partialErrors = append(partialErrors, workErrs...)
	}

	// Count mail (best-effort).
	var mc mailCounts
	seenProvs := make(map[string]bool)
	for _, mp := range s.state.MailProviders() {
		key := fmt.Sprintf("%p", mp)
		if seenProvs[key] {
			continue
		}
		seenProvs[key] = true
		if total, unread, err := statusMailCountWithTimeout(mp); err == nil {
			mc.Total += total
			mc.Unread += unread
		} else {
			partialErrors = append(partialErrors, fmt.Sprintf("mail: %v", err))
		}
	}

	// Collect named sessions (best-effort; skip when unavailable).
	var namedSessionDetails []StatusNamedSessionDetail
	for _, ns := range cfg.NamedSessions {
		identity := ns.QualifiedName()
		mode := ns.ModeOrDefault()
		status := s.namedSessionStatus(cfg, sessionSnapshot, cityName, identity, mode, suspendedRigs)
		namedSessionDetails = append(namedSessionDetails, StatusNamedSessionDetail{
			Identity: identity,
			Status:   status,
			Mode:     mode,
		})
	}

	// Session counts: walk the city bead store for session beads. Omitted in
	// lite mode (detail block, not needed for the high-frequency overview).
	var sessionCounts *StatusSessionCountsDetail
	if !lite && len(sessionSnapshot.bySessionName) > 0 {
		active, suspended := s.countSessions(sessionSnapshot)
		if active > 0 || suspended > 0 {
			sessionCounts = &StatusSessionCountsDetail{Active: active, Suspended: suspended}
		}
	}

	uptime := int(time.Since(s.state.StartedAt()).Seconds())
	versions := s.resolveComponentVersions()

	// StoreHealth carries a full closed-history Dolt row scan (behind a 30s
	// sub-cache). Omitted in lite mode so a cold lite poll never triggers it.
	var storeHealth *StatusStoreHealth
	if !lite {
		storeHealth = s.cachedStoreHealth(time.Now())
	}

	return StatusBody{
		Name:                cityName,
		Path:                s.state.CityPath(),
		Version:             s.state.Version(),
		DoltVersion:         versions.Dolt,
		BeadsVersion:        versions.Beads,
		UptimeSec:           uptime,
		Suspended:           suspensionstate.EffectiveCitySuspended(citySt, cfg.Workspace.EffectiveSuspendedOnStart()),
		AgentCount:          ac.Total,
		RigCount:            rc.Total,
		Running:             rawRunning,
		Agents:              ac,
		Rigs:                rc,
		Work:                wc,
		Mail:                mc,
		Partial:             len(partialErrors) > 0,
		PartialErrors:       partialErrors,
		StoreHealth:         storeHealth,
		Beads:               s.cityBeadsDiagnostic(),
		AgentDetails:        agentDetails,
		RigDetails:          rigDetails,
		NamedSessionDetails: namedSessionDetails,
		SessionCountsDetail: sessionCounts,
	}
}

type cityBeadsDiagnosticProvider interface {
	CityBeadsDiagnostic() *beads.BeadsDiagnostic
}

func (s *Server) cityBeadsDiagnostic() *beads.BeadsDiagnostic {
	provider, ok := s.state.(cityBeadsDiagnosticProvider)
	if !ok {
		return nil
	}
	return provider.CityBeadsDiagnostic()
}

// poolScaleLabel renders the "scaled (min=N, max=M)" banner the CLI emits
// once per pool group. Mirrors the label buildCityStatusSnapshot emits
// client-side so human output is identical whether served via API or
// fallback.
func poolScaleLabel(a config.Agent) string {
	minSessions := 0
	if a.MinActiveSessions != nil {
		minSessions = *a.MinActiveSessions
	}
	maxSessions := 1
	maxLabel := fmt.Sprintf("max=%d", maxSessions)
	if a.MaxActiveSessions != nil {
		maxSessions = *a.MaxActiveSessions
		if maxSessions < 0 {
			maxLabel = "max=unlimited"
		} else {
			maxLabel = fmt.Sprintf("max=%d", maxSessions)
		}
	}
	return fmt.Sprintf("scaled (min=%d, %s)", minSessions, maxLabel)
}

// namedSessionStatus classifies a named session for the StatusBody detail
// block. Mirrors the CLI's namedSessionStatusForCity: reserved when the
// session bead does not resolve, "degraded blocked" when the session is
// always-on but its agent template is blocked by suspension, or the
// session's state metadata when a bead is present.
func (s *Server) namedSessionStatus(
	cfg *config.City,
	snapshot statusSessionSnapshot,
	cityName, identity, mode string,
	suspendedRigs map[string]bool,
) string {
	status := "reserved-unmaterialized"
	if spec := config.FindNamedSession(cfg, identity); spec != nil {
		if mode == "always" && namedSessionTemplateBlocked(cfg, spec, suspendedRigs) {
			status = "degraded blocked"
		}
	}

	runtimeName := config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity)
	if info, ok := snapshot.bySessionName[runtimeName]; ok {
		if info.state != "" {
			return string(info.state)
		}
		return "materialized"
	}
	if len(snapshot.partialErrors) > 0 {
		return "lookup error: " + strings.Join(snapshot.partialErrors, "; ")
	}
	return status
}

// namedSessionTemplateBlocked reports whether a named-session's target
// agent template is blocked by suspension (city suspended, agent template
// suspended, or the template's rig is suspended).
func namedSessionTemplateBlocked(cfg *config.City, ns *config.NamedSession, suspendedRigs map[string]bool) bool {
	if cfg == nil {
		return false
	}
	if cfg.Workspace.Suspended {
		return true
	}
	if ns == nil {
		return false
	}
	for _, a := range cfg.Agents {
		if a.Name != ns.Template {
			continue
		}
		if ns.Dir != "" && a.Dir != ns.Dir {
			continue
		}
		if a.Suspended {
			return true
		}
		if a.Dir != "" && suspendedRigs[a.Dir] {
			return true
		}
		return false
	}
	return false
}

// countSessions tallies active / suspended sessions from the status snapshot.
func (s *Server) countSessions(snapshot statusSessionSnapshot) (active, suspended int) {
	for _, info := range snapshot.bySessionName {
		switch info.state {
		case session.StateActive:
			active++
		case session.StateSuspended:
			suspended++
		}
	}
	return active, suspended
}

type statusSessionSnapshot struct {
	bySessionName map[string]statusSessionInfo
	byTemplate    map[string][]statusSessionInfo
	partialErrors []string
}

type statusSessionInfo struct {
	sessionName string
	agentName   string
	template    string
	state       session.State
}

func (s *Server) statusSessionSnapshot() statusSessionSnapshot {
	snapshot := statusSessionSnapshot{
		bySessionName: make(map[string]statusSessionInfo),
		byTemplate:    make(map[string][]statusSessionInfo),
	}
	store := s.state.CityBeadStore()
	if store == nil {
		return snapshot
	}

	type snapshotResult struct {
		rows          []beads.Bead
		partialErrors []string
		err           error
	}
	done := make(chan snapshotResult, 1)
	go func() {
		rows, partialErrors, err := sessionReadModelRows(store)
		done <- snapshotResult{rows: rows, partialErrors: partialErrors, err: err}
	}()

	var rows []beads.Bead
	var partialErrors []string
	var err error
	select {
	case result := <-done:
		rows = result.rows
		partialErrors = result.partialErrors
		err = result.err
	case <-time.After(statusStoreReadTimeout):
		snapshot.partialErrors = []string{fmt.Sprintf("sessions: loading session snapshot timed out after %s", statusStoreReadTimeout)}
		return snapshot
	}

	if err != nil {
		snapshot.partialErrors = []string{fmt.Sprintf("sessions: %v", err)}
		return snapshot
	}
	for _, partialErr := range partialErrors {
		snapshot.partialErrors = append(snapshot.partialErrors, fmt.Sprintf("sessions: %s", partialErr))
	}

	seenSessionName := make(map[string]bool, len(rows))
	for _, b := range rows {
		if b.Status == "closed" {
			continue
		}
		info := statusSessionInfo{
			sessionName: strings.TrimSpace(b.Metadata["session_name"]),
			agentName:   strings.TrimSpace(b.Metadata["agent_name"]),
			template:    strings.TrimSpace(b.Metadata["template"]),
			state:       statusSessionState(b),
		}
		if info.sessionName == "" {
			continue
		}
		if info.state == session.StateArchived {
			continue
		}
		if seenSessionName[info.sessionName] {
			continue
		}
		seenSessionName[info.sessionName] = true
		snapshot.bySessionName[info.sessionName] = info
		if info.template != "" {
			snapshot.byTemplate[info.template] = append(snapshot.byTemplate[info.template], info)
		}
	}
	return snapshot
}

// statusWorkResult is one store's contribution to the work counts.
type statusWorkResult struct {
	wc   workCounts
	errs []string
}

// statusWorkCounts tallies open/ready/in_progress work across all rig
// stores. Stores exposing beads.Counter answer without hydrating rows —
// the caching layer counts matches in memory when its cache is clean
// (#1896) — with the per-store timeout canceling any delegated backing
// query instead of leaking a goroutine that pins a connection.
// Stores without a Counter (or whose Counter cannot answer the query
// shape) keep the legacy hydrating List path. Stores are queried
// concurrently; results aggregate in deterministic rig order.
func (s *Server) statusWorkCounts(ctx context.Context) (workCounts, []string) {
	stores := s.state.BeadStores()
	// sortedRigNames deduplicates rigs sharing one store instance, so each
	// store is counted exactly once.
	rigNames := sortedRigNames(stores)
	results := make([]statusWorkResult, len(rigNames))
	var wg sync.WaitGroup
	for i, rigName := range rigNames {
		wg.Add(1)
		go func(i int, rigName string, store beads.Store) {
			defer wg.Done()
			results[i] = statusStoreWorkCounts(ctx, rigName, store)
		}(i, rigName, stores[rigName])
	}
	wg.Wait()

	var wc workCounts
	var errs []string
	for _, r := range results {
		wc.Open += r.wc.Open
		wc.Ready += r.wc.Ready
		wc.InProgress += r.wc.InProgress
		errs = append(errs, r.errs...)
	}
	return wc, errs
}

// statusStoreWorkCounts counts one store's work beads, preferring the
// hydration-free Counter path. Operational count failures (timeouts,
// connection errors) report a partial error without retrying via List —
// the List scan would hit the same backend and pay the timeout again.
func statusStoreWorkCounts(ctx context.Context, rigName string, store beads.Store) statusWorkResult {
	if counter, ok := store.(beads.Counter); ok {
		wc, err := statusCountWork(ctx, counter)
		if err == nil {
			return statusWorkResult{wc: wc}
		}
		if !errors.Is(err, beads.ErrCountUnsupported) {
			return statusWorkResult{errs: []string{fmt.Sprintf("rig %s work: %v", rigName, err)}}
		}
	}

	list, err := statusListStoreWithTimeout(store, beads.ListQuery{AllowScan: true})
	var result statusWorkResult
	if err != nil {
		result.errs = append(result.errs, fmt.Sprintf("rig %s work: %v", rigName, err))
		if !beads.IsPartialResult(err) || len(list) == 0 {
			return result
		}
	}
	for _, b := range list {
		if slices.Contains(statusWorkExcludedTypes, b.Type) {
			continue
		}
		switch b.Status {
		case "in_progress":
			result.wc.InProgress++
		case "ready":
			result.wc.Ready++
		case "open":
			result.wc.Open++
		}
	}
	return result
}

// statusCountWork fills the work-count buckets via beads.Counter. One
// shared statusStoreReadTimeout window bounds all three bucket queries —
// the same per-store budget the legacy single-List path had, though the
// three queries consume it serially — and derives from ctx, so a slow
// backend query is canceled (releasing its connection) rather than
// abandoned.
func statusCountWork(ctx context.Context, counter beads.Counter) (workCounts, error) {
	ctx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	defer cancel()
	var wc workCounts
	for _, bucket := range []struct {
		status string
		dst    *int
	}{
		{"open", &wc.Open},
		{"ready", &wc.Ready},
		{"in_progress", &wc.InProgress},
	} {
		n, err := counter.Count(ctx, beads.ListQuery{Status: bucket.status, AllowScan: true}, statusWorkExcludedTypes...)
		if err != nil {
			return workCounts{}, err
		}
		*bucket.dst = n
	}
	return wc, nil
}

// statusListStoreWithTimeout lists with the per-store read timeout.
// Store.List takes no context, so on timeout the goroutine is abandoned
// (it keeps its connection until the scan returns). Counter-capable
// stores avoid this path entirely; fixing it for the remaining stores
// requires plumbing context through Store.List.
func statusListStoreWithTimeout(store beads.Store, query beads.ListQuery) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	type listResult struct {
		rows []beads.Bead
		err  error
	}
	done := make(chan listResult, 1)
	go func() {
		rows, err := store.List(query)
		done <- listResult{rows: rows, err: err}
	}()
	select {
	case result := <-done:
		return result.rows, result.err
	case <-time.After(statusStoreReadTimeout):
		return nil, fmt.Errorf("list timed out after %s", statusStoreReadTimeout)
	}
}

func statusMailCountWithTimeout(mp interface {
	Count(string) (total int, unread int, err error)
},
) (int, int, error) {
	if mp == nil {
		return 0, 0, nil
	}
	type countResult struct {
		total  int
		unread int
		err    error
	}
	done := make(chan countResult, 1)
	go func() {
		total, unread, err := mp.Count("")
		done <- countResult{total: total, unread: unread, err: err}
	}()
	select {
	case result := <-done:
		return result.total, result.unread, result.err
	case <-time.After(statusStoreReadTimeout):
		return 0, 0, fmt.Errorf("count timed out after %s", statusStoreReadTimeout)
	}
}

func appendUnlimitedPoolSessionBeads(expanded []expandedAgent, a config.Agent, cityName, sessTmpl string, snapshot statusSessionSnapshot) []expandedAgent {
	maxSess := a.EffectiveMaxActiveSessions()
	if !a.SupportsInstanceExpansion() || (maxSess != nil && *maxSess >= 0) {
		return expanded
	}

	seenSessionNames := make(map[string]bool, len(expanded))
	for _, ea := range expanded {
		seenSessionNames[agentSessionName(cityName, ea.qualifiedName, sessTmpl)] = true
	}

	poolName := a.QualifiedName()
	for _, info := range snapshot.byTemplate[poolName] {
		if seenSessionNames[info.sessionName] {
			continue
		}
		qn := statusSessionQualifiedName(cityName, sessTmpl, info)
		if qn == "" {
			continue
		}
		expanded = append(expanded, expandedAgent{
			qualifiedName: qn,
			rig:           a.Dir,
			pool:          poolName,
			suspended:     a.Suspended,
			provider:      a.Provider,
			description:   a.Description,
		})
		seenSessionNames[info.sessionName] = true
	}
	return expanded
}

func statusSessionQualifiedName(cityName, sessTmpl string, info statusSessionInfo) string {
	if info.agentName != "" && info.agentName != info.template {
		return info.agentName
	}
	qnSanitized := info.sessionName
	templatePrefix := agent.SessionNameFor(cityName, "", sessTmpl)
	if templatePrefix != "" && strings.HasPrefix(qnSanitized, templatePrefix) {
		qnSanitized = qnSanitized[len(templatePrefix):]
	}
	return agent.UnsanitizeQualifiedNameFromSession(qnSanitized)
}

func statusSessionState(b beads.Bead) session.State {
	state := session.State(strings.TrimSpace(b.Metadata["state"]))
	switch state {
	case "awake":
		return session.StateActive
	case "drained":
		return session.StateAsleep
	default:
		return state
	}
}

func statusProviderRunning(sp interface{ IsRunning(string) bool }, sessionName string) bool {
	sessionName = strings.TrimSpace(sessionName)
	if sp == nil || sessionName == "" {
		return false
	}
	return sp.IsRunning(sessionName)
}

// HealthInput is the Huma input for GET /v0/city/{cityName}/health.
type HealthInput struct {
	CityScope
}

// humaHandleHealth is the Huma-typed handler for GET /v0/city/{cityName}/health.
func (s *Server) humaHandleHealth(_ context.Context, _ *HealthInput) (*HealthOutput, error) {
	uptime := int(time.Since(s.state.StartedAt()).Seconds())
	out := &HealthOutput{}
	out.Body.Status = "ok"
	out.Body.Version = s.state.Version()
	out.Body.City = s.state.CityName()
	out.Body.UptimeSec = uptime
	return out, nil
}

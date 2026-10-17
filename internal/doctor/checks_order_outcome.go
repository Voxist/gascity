package doctor

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	orderOutcomeHealthyName = "order-outcome-healthy"

	// orderOutcomeInspectHintFmt mirrors the sibling check's
	// orderFiringInspectHintFmt so the pair emits consistently-shaped hints.
	orderOutcomeInspectHintFmt = "Inspect with: gc order check && gc order history %s"

	// orderOutcomeFailureThreshold is the consecutive-failure count that flags an
	// order. Three, not two: two in a row is a plausible transient for anything
	// touching the network or a lock. Three, not five: on a 6h order five failures
	// is 30h before anything surfaces.
	orderOutcomeFailureThreshold = 3

	// orderOutcomeStartGrace covers gastownhall/gascity#3898 — for ~5 minutes
	// after a supervisor start, exec orders fail spuriously because dispatch
	// begins before pack staging completes. 2x margin on the observed window.
	orderOutcomeStartGrace = 10 * time.Minute

	// eventLogSinceFloor is the floor sinceWindowFor's derived Since window
	// never shrinks below, regardless of how short the widest monitored
	// interval is. On a city whose shortest order is a few minutes, 3x that
	// alone would prune almost everything, leaving too little trailing
	// history for order-outcome-healthy's failure-streak threshold
	// (orderOutcomeFailureThreshold) to mean anything.
	eventLogSinceFloor = 6 * time.Hour
)

// sinceWindowFor derives a bounded Since timestamp as
// now - max(3 x maxExpected, eventLogSinceFloor).
//
// Provably lossless for order-outcome-healthy's trailing-failure count: every
// monitored cron/cooldown order emits an outcome at least every expected
// interval, so a genuine trailing streak of orderOutcomeFailureThreshold
// failures sits entirely inside 3x the widest expected interval.
//
// order-firing-current's own classifyOrderFiring already declares CRITICAL
// stale at the same age >= expected*3 boundary (vc-89s), so the identical
// window is lossless there too — no fire older than it can change that
// check's verdict either. Shared by both checks (via this one function) so
// their Since windows can never drift apart.
func sinceWindowFor(maxExpected time.Duration, now time.Time) time.Time {
	window := 3 * maxExpected
	if window < eventLogSinceFloor {
		window = eventLogSinceFloor
	}
	return now.Add(-window)
}

// nearControllerStart reports whether ts falls within grace after any controller
// start. Every start is checked, not just the newest: two restarts with no
// successful run between them would otherwise leave the older burst counted and
// manufacture a false positive.
func nearControllerStart(ts time.Time, starts []time.Time, grace time.Duration) bool {
	if grace <= 0 {
		return false
	}
	for _, start := range starts {
		if start.IsZero() || ts.Before(start) {
			continue
		}
		if ts.Sub(start) <= grace {
			return true
		}
	}
	return false
}

// consecutiveOrderFailures counts trailing order.failed events for one order.
//
// outcomes must hold order.completed and order.failed events ordered by Seq
// ascending; the walk runs newest-first and stops at the first success.
//
// A success always ends the streak, even if it falls within the post-start grace
// window. The grace window skips only spurious FAILURES (neither counting them nor
// allowing them to break the streak); a success is proof the order works.
//
// Failures inside the post-start grace window are SKIPPED, not reset. Resetting
// would let a frequently-restarting city zero a genuinely broken order's streak
// on every restart, which is the opposite of what this check is for.
//
// sawOutcome distinguishes "ran and succeeded" from "never produced an outcome";
// order-firing-current already owns the never-fired case. Since the read is
// Since-bounded (vc-sen0), sawOutcome=false means "no outcome within the
// derived window", not "no outcome ever" — correct by construction, because a
// monitored order with zero outcomes in 3x its expected interval is stale,
// which is order-firing-current's jurisdiction, not this check's.
//
// skipped counts trailing failures that were inside the post-start grace
// window and therefore excluded from streak. Callers need this to avoid
// reporting "last run succeeded" when every trailing run actually failed but
// was suppressed as a spurious post-restart burst — see classifyOrderOutcome.
func consecutiveOrderFailures(outcomes []events.Event, subject string, starts []time.Time, grace time.Duration) (streak int, lastMessage string, sawOutcome bool, skipped int) {
	lastMessageSet := false

	for i := len(outcomes) - 1; i >= 0; i-- {
		event := outcomes[i]
		if event.Subject != subject {
			continue
		}
		sawOutcome = true
		if event.Type != events.OrderFailed {
			break
		}
		if nearControllerStart(event.Ts, starts, grace) {
			skipped++
			continue
		}
		streak++
		if !lastMessageSet {
			lastMessage = event.Message
			lastMessageSet = true
		}
	}

	return streak, lastMessage, sawOutcome, skipped
}

// classifyOrderOutcome turns one order's failure streak into a doctor result.
//
// skipped is the grace-window-skipped-failure count from consecutiveOrderFailures.
// When streak == 0 but skipped > 0, every trailing run actually failed (just
// inside the post-start grace window); reporting "last run succeeded" would be
// a false statement in the diagnostic tool at exactly the moment an operator is
// most likely reading it — just after a restart.
func classifyOrderOutcome(order orders.Order, streak int, threshold int, lastMessage string, sawOutcome bool, skipped int) (CheckStatus, string) {
	name := orderDisplayName(order)

	if !sawOutcome {
		return StatusOK, fmt.Sprintf("%s: no completed runs yet", name)
	}
	if streak == 0 {
		if skipped > 0 {
			return StatusOK, fmt.Sprintf("%s: %d recent failure(s) within controller-start grace window", name, skipped)
		}
		return StatusOK, fmt.Sprintf("%s: last run succeeded", name)
	}
	if streak < threshold {
		return StatusOK, fmt.Sprintf("%s: %d consecutive failure(s), under threshold %d", name, streak, threshold)
	}

	detail := fmt.Sprintf("%s: %d consecutive failures", name, streak)
	if strings.TrimSpace(lastMessage) != "" {
		detail = fmt.Sprintf("%s, last %q", detail, lastMessage)
	}
	return StatusWarning, detail
}

// OrderOutcomeHealthyCheck reports scheduled orders failing repeatedly.
//
// Sibling to OrderFiringCurrentCheck, which answers "did it run?" while this
// answers "did it succeed?". An order that fires faithfully on schedule and fails
// every time leaves order-firing-current green, so the failure is invisible: the
// only trace is order.failed in the event log, which nobody reads unprompted.
type OrderOutcomeHealthyCheck struct {
	cfg       *config.City
	cityPath  string
	threshold int
	grace     time.Duration
	// clock is overridden in tests that need to pin "now" against fixture
	// timestamps; production always uses time.Now.
	clock func() time.Time
}

// NewOrderOutcomeHealthyCheck creates the repeated-order-failure check.
func NewOrderOutcomeHealthyCheck(cfg *config.City, cityPath string) *OrderOutcomeHealthyCheck {
	return &OrderOutcomeHealthyCheck{
		cfg:       cfg,
		cityPath:  cityPath,
		threshold: orderOutcomeFailureThreshold,
		grace:     orderOutcomeStartGrace,
		clock:     time.Now,
	}
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderOutcomeHealthyCheck) Name() string { return orderOutcomeHealthyName }

// CanFix reports whether the check can repair a failing order. It cannot:
// remediation depends entirely on why the order fails.
func (c *OrderOutcomeHealthyCheck) CanFix() bool { return false }

// Fix is a no-op for the reason given on CanFix.
func (c *OrderOutcomeHealthyCheck) Fix(_ *CheckContext) error { return nil }

// Run counts each scheduled order's trailing consecutive failures.
//
// Unlike order-firing-current this needs no goroutine-plus-timeout guard: that
// check wraps its work because the order-history resolver opens the beads/Dolt
// store without accepting a context. This one reads only the event log.
func (c *OrderOutcomeHealthyCheck) Run(ctx *CheckContext) *CheckResult {
	// This check is advisory on every path: a failing order must not gate gc doctor.
	// Blocking would fail gc doctor outright and gate every clean-doctor dependency
	// on transient order breakage, including during maintenance. SeverityAdvisory
	// is the only source of truth, set here at construction.
	result := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}
	if c.cfg == nil {
		result.Status = StatusOK
		result.Message = "no city config loaded"
		return result
	}

	cityPath := c.cityPath
	if cityPath == "" && ctx != nil {
		cityPath = ctx.CityPath
	}
	if cityPath == "" {
		result.Status = StatusError
		result.Message = "city path unavailable"
		return result
	}

	// Same helper order-firing-current uses, so the two checks can never
	// disagree about which orders are in scope.
	allOrders, err := scanOrderFiringCurrentOrders(cityPath, c.cfg)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("scan orders: %v", err)
		return result
	}

	now := c.clock()
	if now.IsZero() {
		now = time.Now()
	}

	// Resolve the Since window BEFORE reading the event log, from the same
	// trigger-type gate the monitoring loop below applies, so the window and
	// the monitored set can never disagree about which orders count. A
	// suspended-rig order is deliberately still counted here: including its
	// interval in the window can only widen it (never lossy), and the loop
	// below is what actually excludes suspended orders from the result.
	cronCache := map[string]time.Duration{}
	var maxExpected time.Duration
	for _, order := range allOrders {
		if order.Trigger != "cron" && order.Trigger != "cooldown" {
			continue
		}
		expected, intervalErr := expectedIntervalForOrder(order, cronCache)
		if intervalErr != nil {
			continue
		}
		if expected > maxExpected {
			maxExpected = expected
		}
	}
	since := sinceWindowFor(maxExpected, now)

	eventPath := filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
	outcomes, err := readOrderOutcomeEvents(eventPath, since)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read order outcome events: %v", err)
		return result
	}
	// since minus grace, not since: a controller start that itself falls just
	// before the outcome window's edge can still mark an outcome inside that
	// window as "near start" (nearControllerStart looks forward from a start
	// by grace), so a start within [since-grace, since) must still be visible.
	starts, err := controllerStartTimes(eventPath, since.Add(-c.grace))
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read controller start events: %v", err)
		return result
	}

	worst := StatusOK
	monitored := 0
	failing := 0
	var firstFailingHint string
	suspendedRigs := orderFiringCurrentSuspendedRigs(c.cfg, cityPath)

	for _, order := range allOrders {
		// Manual and event-triggered orders are out of scope by construction,
		// matching the sibling check. This is what keeps a manual order that was
		// abandoned mid-failure from alarming forever, with no recency heuristic
		// needed: it simply is not in the monitored set.
		if order.Trigger != "cron" && order.Trigger != "cooldown" {
			continue
		}
		if orderFiringCurrentOrderSuspended(suspendedRigs, order) {
			continue
		}
		monitored++

		streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(outcomes, order.ScopedName(), starts, c.grace)
		status, detail := classifyOrderOutcome(order, streak, c.threshold, lastMessage, sawOutcome, skipped)
		worst = worseStatus(worst, status)
		result.Details = append(result.Details, detail)
		if status != StatusOK {
			failing++
			if firstFailingHint == "" {
				// gc order history takes a bare name positionally and filters
				// on a.Name; the scoped form matches zero orders on the
				// local-iterator path used when the supervisor API is
				// unavailable — exactly when someone is debugging a broken
				// city. orderHistoryHintTarget yields the --rig form instead.
				firstFailingHint = orderHistoryHintTarget(order)
			}
		}
	}

	if monitored == 0 {
		result.Status = StatusOK
		result.Message = "no cron or cooldown orders"
		return result
	}

	result.Status = worst
	if worst == StatusOK {
		result.Message = "all scheduled orders succeeding"
	} else {
		result.Message = fmt.Sprintf("%d order(s) failing repeatedly", failing)
		result.FixHint = fmt.Sprintf(orderOutcomeInspectHintFmt, firstFailingHint)
	}
	return result
}

// readOrderOutcomeEvents returns order.completed and order.failed merged in Seq
// order, bounded to events at or after since. events.Filter matches a single
// Type, hence two reads.
//
// Since-bounding an archived chain lets archiveOverlapsFilter skip archives
// that predate the window by name, without opening them (vc-sen0): an
// unbounded read here previously streamed and gunzipped the entire archive
// chain on every doctor run (measured ~7.9GB / ~56 archives), which alone blew
// the check's time budget regardless of the live log's size.
func readOrderOutcomeEvents(eventPath string, since time.Time) ([]events.Event, error) {
	completed, err := events.ReadFiltered(eventPath, events.Filter{Type: events.OrderCompleted, Since: since})
	if err != nil {
		return nil, err
	}
	failed, err := events.ReadFiltered(eventPath, events.Filter{Type: events.OrderFailed, Since: since})
	if err != nil {
		return nil, err
	}
	merged := make([]events.Event, 0, len(completed)+len(failed))
	merged = append(merged, completed...)
	merged = append(merged, failed...)
	// Seq, not Ts: the log is append-only and seq-ordered, and two events in the
	// same second would otherwise sort arbitrarily.
	sort.Slice(merged, func(i, j int) bool { return merged[i].Seq < merged[j].Seq })
	return merged, nil
}

// controllerStartTimes returns every controller.started timestamp at or after
// since. The sibling check's latestControllerStartedAt returns only the
// newest, which is not enough here — see nearControllerStart. Callers pass a
// since bounded further back than the outcome window by the post-start grace
// margin (see the call site in Run), not the outcome window's own since.
func controllerStartTimes(eventPath string, since time.Time) ([]time.Time, error) {
	startEvents, err := events.ReadFiltered(eventPath, events.Filter{Type: events.ControllerStarted, Since: since})
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, len(startEvents))
	for _, event := range startEvents {
		out = append(out, event.Ts)
	}
	return out, nil
}

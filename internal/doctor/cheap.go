package doctor

// CheapCheck is an optional marker interface for checks inexpensive enough
// to run at supervisor cadence (startup and a fixed interval), in addition
// to on-demand `gc doctor` runs. "Cheap" means: no store opens, no bd
// subprocesses against live data, bounded to small local file reads and
// short read-only commands. The supervisor doctor subset (City-Scale plan
// item 1.9) consumes this marker via CheapChecks; checks opt in by
// implementing Cheap() returning true.
type CheapCheck interface {
	// Cheap reports whether this check belongs to the supervisor-cadence
	// subset.
	Cheap() bool
}

// IsCheap reports whether c opts in to the supervisor-cadence cheap subset.
func IsCheap(c Check) bool {
	cc, ok := c.(CheapCheck)
	return ok && cc.Cheap()
}

// CheapChecks filters checks to those that opt in via CheapCheck,
// preserving order. Intended for the supervisor's startup / fixed-interval
// doctor evaluation, which must never pay for store-touching checks.
func CheapChecks(checks []Check) []Check {
	var cheap []Check
	for _, c := range checks {
		if IsCheap(c) {
			cheap = append(cheap, c)
		}
	}
	return cheap
}

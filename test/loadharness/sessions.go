//go:build loadharness

package loadharness

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// sessionFleet materializes a scope's synthetic open sessions on the
// fake runtime provider. The plan's per-assignee live Ready fan-out is driven
// by the count of open sessions; modeling them as real fake-provider sessions
// (rather than a bare integer) keeps the harness honest about where the
// fan-out comes from and lets the tick iterate actual session names.
type sessionFleet struct {
	provider *runtime.Fake
	names    []string
}

// newSessionFleet starts n fake sessions named for the scope and returns the
// fleet. The provider is the in-memory runtime.Fake — no real process or tmux
// is involved.
func newSessionFleet(scope string, n int) *sessionFleet {
	p := runtime.NewFake()
	names := make([]string, 0, n)
	for a := 0; a < n; a++ {
		name := fmt.Sprintf("%s-agent-%d", scope, a)
		if err := p.Start(context.Background(), name, runtime.Config{}); err != nil {
			panic(fmt.Sprintf("loadharness: starting fake session %s: %v", name, err))
		}
		names = append(names, name)
	}
	return &sessionFleet{provider: p, names: names}
}

// assignees returns the session names that drive the per-assignee Ready
// fan-out for the scope.
func (f *sessionFleet) assignees() []string {
	return f.names
}

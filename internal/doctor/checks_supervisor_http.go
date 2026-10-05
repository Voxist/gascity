package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// httpDoer abstracts the HTTP client so tests can inject a mock.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// SupervisorHTTPCheck verifies that the supervisor HTTP API is reachable on
// its configured port. The check is skipped when the supervisor unix socket
// is already known to be down so the operator sees one clear problem rather
// than two.
type SupervisorHTTPCheck struct {
	supervisorRunning bool
	loadConfig        func(string) (supervisor.Config, error)
	configPath        func() string
	client            httpDoer
}

// NewSupervisorHTTPCheck returns a check configured to probe the supervisor
// HTTP API. supervisorRunning should come from the unix-socket probe result.
func NewSupervisorHTTPCheck(supervisorRunning bool) *SupervisorHTTPCheck {
	return &SupervisorHTTPCheck{
		supervisorRunning: supervisorRunning,
		loadConfig:        supervisor.LoadConfig,
		configPath:        supervisor.ConfigPath,
		client:            &http.Client{Timeout: 3 * time.Second},
	}
}

// Name returns the check identifier.
func (c *SupervisorHTTPCheck) Name() string { return "supervisor-http-api" }

// CanFix reports that this check does not support automatic remediation.
func (c *SupervisorHTTPCheck) CanFix() bool { return false }

// Fix is a no-op; CanFix returns false.
func (c *SupervisorHTTPCheck) Fix(_ *CheckContext) error { return nil }

// Run checks that the supervisor HTTP API is reachable on its configured port.
func (c *SupervisorHTTPCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if !c.supervisorRunning {
		r.Status = StatusOK
		r.Message = "supervisor socket not running — HTTP API check skipped"
		return r
	}

	cfg, err := c.loadConfig(c.configPath())
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("cannot load supervisor config: %v", err)
		return r
	}
	port := cfg.Supervisor.PortOrDefault()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/v0/cities", port), nil)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("supervisor HTTP API on port %d: %v", port, err)
		return r
	}

	resp, err := c.client.Do(req)
	if err != nil {
		if isPortExhaustion(err) {
			// Client-side ephemeral-port exhaustion (EADDRNOTAVAIL), not
			// evidence the supervisor is down: the OS couldn't find a free
			// local port to originate this dial, which says nothing about
			// whether anything is listening on the far end (ga-4k2m7).
			// StatusError here would read as "supervisor unreachable" during
			// exactly the host-wide port-churn storms this check should stay
			// quiet about; StatusWarning says the probe itself was
			// inconclusive, consistent with #227 treating EADDRNOTAVAIL as not
			// death evidence.
			r.Status = StatusWarning
			r.Message = fmt.Sprintf("supervisor HTTP API on port %d: probe inconclusive — client-side ephemeral-port exhaustion (EADDRNOTAVAIL), likely host connection churn; not a liveness signal", port)
			return r
		}
		if isConnectionRefused(err) {
			r.Status = StatusError
			r.Message = fmt.Sprintf("supervisor HTTP API on port %d: connection refused", port)
			return r
		}
		if isTimeout(err) {
			r.Status = StatusError
			r.Message = fmt.Sprintf("supervisor HTTP API on port %d: timeout", port)
			return r
		}
		r.Status = StatusError
		r.Message = fmt.Sprintf("supervisor HTTP API on port %d: %v", port, err)
		return r
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort body close

	if resp.StatusCode/100 != 2 {
		r.Status = StatusError
		r.Message = fmt.Sprintf("supervisor HTTP API on port %d: non-2xx HTTP %d", port, resp.StatusCode)
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("supervisor socket OK, HTTP API reachable on port %d", port)
	return r
}

func isConnectionRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(err.Error(), "connection refused")
}

// isPortExhaustion reports whether err is EADDRNOTAVAIL — the OS refusing to
// originate a connection because it found no free local ephemeral port, NOT
// because anything refused or timed out on the far end. Under a host-wide
// connection-churn storm (ga-4k2m7: dozens of orders failed this way when
// voxist-city's Dolt client connections exhausted the ephemeral range) this
// can hit ANY outbound dial from the process, including this check's own
// probe of the supervisor's HTTP port — a client-side symptom that must not
// be read as the supervisor being down.
func isPortExhaustion(err error) bool {
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		return true
	}
	// macOS and Linux word the same errno differently.
	msg := err.Error()
	return strings.Contains(msg, "can't assign requested address") ||
		strings.Contains(msg, "cannot assign requested address")
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

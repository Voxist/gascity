// Package providergov implements the provider governor: quota-aware,
// capability-aware scheduling signals for multi-account Claude fleets.
//
// Phase 1 scope (city-scale architecture plan, "Provider Governor"):
// a controller-side quota poller that reads each Claude account's
// subscription usage from the OAuth usage endpoint and emits typed
// provider.quota_observed / provider.quota_poll_failed events, plus the
// pure SelectProvider decision function. Nothing here touches the live
// spawn path; wiring decisions into session lifecycle is Phase 2.
//
// ZFC note: this package measures state and applies configured
// thresholds (Policy). It contains no heuristics — every branch in the
// decision function is a mechanical comparison against config.
package providergov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the production Anthropic API origin serving the
	// OAuth usage endpoint.
	DefaultBaseURL = "https://api.anthropic.com"
	// usagePath is the subscription-usage endpoint. It returns usage
	// metadata only; polling consumes zero model tokens.
	usagePath = "/api/oauth/usage"
	// oauthBetaValue is the anthropic-beta header value required by the
	// OAuth usage endpoint.
	oauthBetaValue = "oauth-2025-04-20"
	// maxUsageBodyBytes bounds how much of a usage (or error) response
	// body is read. The real payload is a few hundred bytes.
	maxUsageBodyBytes = 1 << 20
)

// Reason classes for provider.quota_poll_failed payloads. Each is a
// mechanical classification of the failure mode; the human-readable
// cause travels on the event envelope's Message field.
const (
	// ReasonClassCredential: the monitoring credential file is missing,
	// unreadable, or carries no access token.
	ReasonClassCredential = "credential"
	// ReasonClassAuth: the endpoint rejected the token (401/403) — e.g.
	// a setup-token without the user:profile scope, or an expired login.
	// V1 emits the degraded signal and does not attempt a refresh.
	ReasonClassAuth = "auth"
	// ReasonClassTimeout: the request exceeded its deadline.
	ReasonClassTimeout = "timeout"
	// ReasonClassNetwork: transport-level failure (DNS, connect, TLS).
	ReasonClassNetwork = "network"
	// ReasonClassHTTPError: a non-2xx status other than 401/403.
	ReasonClassHTTPError = "http_error"
	// ReasonClassDecode: the response body was not the expected JSON.
	ReasonClassDecode = "decode"
)

// PollError is a quota poll failure with its mechanical reason class.
type PollError struct {
	// ReasonClass is one of the ReasonClass* constants.
	ReasonClass string
	// Err is the underlying cause.
	Err error
}

// Error renders the class-prefixed cause.
func (e *PollError) Error() string {
	return fmt.Sprintf("%s: %v", e.ReasonClass, e.Err)
}

// Unwrap exposes the underlying cause for errors.Is/As.
func (e *PollError) Unwrap() error { return e.Err }

// UsageWindow is one rate-limit bucket of the usage response.
// Utilization is percent-of-cap used (remaining = 100 − utilization);
// ResetsAt is the exact window reset time, nil when the API reports
// null (e.g. an untouched bucket).
type UsageWindow struct {
	Utilization float64    `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

// UsageSnapshot is the typed shape of GET /api/oauth/usage. Buckets the
// API reports as null decode to nil pointers (seven_day_opus and
// seven_day_sonnet are per-model weekly buckets that may be absent).
type UsageSnapshot struct {
	FiveHour       *UsageWindow `json:"five_hour"`
	SevenDay       *UsageWindow `json:"seven_day"`
	SevenDayOpus   *UsageWindow `json:"seven_day_opus"`
	SevenDaySonnet *UsageWindow `json:"seven_day_sonnet"`
}

// FetchUsage reads one account's subscription usage from the OAuth
// usage endpoint. token must be a full OAuth login credential carrying
// the user:profile scope (agent setup-tokens are rejected with 403).
// client defaults to http.DefaultClient and baseURL to DefaultBaseURL.
// Failures are returned as *PollError with a mechanical ReasonClass.
func FetchUsage(ctx context.Context, client *http.Client, baseURL, token string) (UsageSnapshot, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+usagePath, nil)
	if err != nil {
		return UsageSnapshot{}, &PollError{ReasonClass: ReasonClassNetwork, Err: fmt.Errorf("building usage request: %w", err)}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBetaValue)

	resp, err := client.Do(req)
	if err != nil {
		class := ReasonClassNetwork
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			class = ReasonClassTimeout
		}
		return UsageSnapshot{}, &PollError{ReasonClass: class, Err: fmt.Errorf("fetching usage: %w", err)}
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageBodyBytes))
	if err != nil {
		return UsageSnapshot{}, &PollError{ReasonClass: ReasonClassNetwork, Err: fmt.Errorf("reading usage response: %w", err)}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return UsageSnapshot{}, &PollError{
			ReasonClass: ReasonClassAuth,
			Err:         fmt.Errorf("usage endpoint returned %d: %s", resp.StatusCode, truncateBody(body)),
		}
	case resp.StatusCode != http.StatusOK:
		return UsageSnapshot{}, &PollError{
			ReasonClass: ReasonClassHTTPError,
			Err:         fmt.Errorf("usage endpoint returned %d: %s", resp.StatusCode, truncateBody(body)),
		}
	}

	var snap UsageSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return UsageSnapshot{}, &PollError{ReasonClass: ReasonClassDecode, Err: fmt.Errorf("decoding usage response: %w", err)}
	}
	return snap, nil
}

// truncateBody bounds an error-body excerpt for error messages.
func truncateBody(body []byte) string {
	const maxExcerpt = 200
	s := strings.TrimSpace(string(body))
	if len(s) > maxExcerpt {
		return s[:maxExcerpt] + "…"
	}
	return s
}

// monitorCredentialFile is the credential file name inside a
// monitor_config_dir, as written by `CLAUDE_CONFIG_DIR=<dir> claude login`.
const monitorCredentialFile = ".credentials.json"

// monitorCredentials documents the on-disk JSON shape of the monitoring
// credential file. Only the access token is read; refresh handling is
// out of scope for v1 (an expired token surfaces as ReasonClassAuth).
//
//	{ "claudeAiOauth": { "accessToken": "sk-ant-oat…", … } }
type monitorCredentials struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

// ReadMonitorToken reads the OAuth access token from
// <dir>/.credentials.json. Returns an error when the file is missing,
// malformed, or carries an empty access token.
func ReadMonitorToken(dir string) (string, error) {
	path := filepath.Join(dir, monitorCredentialFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading monitoring credential %s: %w", path, err)
	}
	var creds monitorCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("parsing monitoring credential %s: %w", path, err)
	}
	token := strings.TrimSpace(creds.ClaudeAiOauth.AccessToken)
	if token == "" {
		return "", fmt.Errorf("monitoring credential %s has no claudeAiOauth.accessToken", path)
	}
	return token, nil
}

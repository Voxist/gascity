// model_proxy.go — in-process HTTP reverse proxy that routes model API
// requests to the configured provider upstream and feeds status codes into
// the provider-health registry.
//
// URL shape: /proxy/{provider}/v1/...  (provider name must be URL-safe)
// The leading /proxy/{provider} segment is stripped before forwarding.
package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const defaultAnthropicBaseURL = "https://api.anthropic.com"

// newModelProxyHandler returns an http.Handler that routes each request by its
// provider segment, reverse-proxies it to the configured upstream, and records
// the HTTP status in reg.
//
// providerUpstreams maps provider name (e.g. "claude") to its base URL
// (e.g. "https://api.anthropic.com"). Providers absent from the map fall back
// to defaultAnthropicBaseURL.
func newModelProxyHandler(reg *providerHealthRegistry, providerUpstreams map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider, rest := splitProviderPath(r.URL.Path)
		if provider == "" {
			http.Error(w, "missing provider in path", http.StatusBadRequest)
			return
		}

		upstreamBase := defaultAnthropicBaseURL
		if u, ok := providerUpstreams[provider]; ok && u != "" {
			upstreamBase = u
		}

		target, err := url.Parse(upstreamBase)
		if err != nil {
			http.Error(w, "invalid upstream URL", http.StatusBadGateway)
			return
		}

		rp := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = target.Scheme
				req.URL.Host = target.Host
				req.URL.Path = rest
				req.Host = target.Host
			},
			ModifyResponse: func(resp *http.Response) error {
				reg.RecordResponse(provider, resp.StatusCode, time.Now())
				return nil
			},
			ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, err error) {
				// Treat connection errors as a server-side error but do not
				// record them as a health signal — only HTTP statuses count.
				http.Error(rw, "upstream error: "+err.Error(), http.StatusBadGateway)
			},
		}
		rp.ServeHTTP(w, r)
	})
}

// splitProviderPath extracts the provider name from /proxy/{provider}/rest
// and returns ("", "") for paths that don't match.
func splitProviderPath(path string) (provider, rest string) {
	// path must start with /proxy/
	after, ok := strings.CutPrefix(path, "/proxy/")
	if !ok {
		return "", ""
	}
	idx := strings.IndexByte(after, '/')
	if idx < 0 {
		// /proxy/provider — no rest path
		return after, "/"
	}
	return after[:idx], after[idx:]
}

package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The CLI's status reads are served the server's warm StatusView at once
// (vp-e0hv): GetStatus sends no ?fresh=true, and none of the poller or
// long-poll parameters. A fresh read waits up to statusFreshBuildWait for a
// build, which is the wait the warm view exists to take off gc status; the
// age and refresh state the server reports with the warm body say how old it
// is. Explicit ?fresh=true API requests keep upstream's build slots.
func TestGetStatusReadsTheWarmBody(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"c","path":"/c","uptime_sec":1,"suspended":false,"agent_count":0,"rig_count":0,"running":0,` +
			`"agents":{"total":0,"running":0,"suspended":0,"quarantined":0},"rigs":{"total":0,"suspended":0},` +
			`"work":{"in_progress":0,"ready":0,"open":0},"mail":{"unread":0,"total":0}}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := NewCityScopedClient(srv.URL, "c").GetStatus(); err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetStatus query = %v, want none (no fresh=true: the CLI reads the warm body)", got)
	}
}

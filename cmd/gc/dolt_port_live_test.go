package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func fakeLayoutFn(layout managedDoltRuntimeLayout) func(string) (managedDoltRuntimeLayout, error) {
	return func(string) (managedDoltRuntimeLayout, error) { return layout, nil }
}

func testManagedLayout() managedDoltRuntimeLayout {
	return managedDoltRuntimeLayout{
		DataDir:    "/city/.beads/dolt",
		ConfigFile: "/city/.gc/runtime/packs/dolt/config.yaml",
	}
}

func attemptStatuses(attempts []PortResolutionAttempt) []string {
	out := make([]string, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, a.Source+":"+a.Status)
	}
	return out
}

func TestLiveDoltPortResolver_ManagedHandleWins(t *testing.T) {
	r := liveDoltPortResolver{
		managedHandlePort: func(cityPath string) string {
			if cityPath != "/city" {
				t.Errorf("managedHandlePort cityPath = %q, want /city", cityPath)
			}
			return "28231"
		},
		runtimeLayout: fakeLayoutFn(testManagedLayout()),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			t.Error("process table consulted although the managed handle resolved")
			return nil, nil
		},
	}

	got, err := r.resolve("/city")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Port != 28231 {
		t.Errorf("Port = %d, want 28231", got.Port)
	}
	if got.Source != liveDoltHandleSource {
		t.Errorf("Source = %q, want %q", got.Source, liveDoltHandleSource)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].Status != "found" {
		t.Errorf("Attempts = %v, want single found entry", attemptStatuses(got.Attempts))
	}
}

func TestLiveDoltPortResolver_ProcessTableByConfigPath(t *testing.T) {
	layout := testManagedLayout()
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(layout),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{
				// Foreign server: different config, must be ignored.
				{PID: 1, Ports: []int{4000}, Argv: []string{"dolt", "sql-server", "--config", "/elsewhere/config.yaml"}},
				{PID: 2, Ports: []int{28231}, Argv: []string{"dolt", "sql-server", "--config", layout.ConfigFile}},
			}, nil
		},
	}

	got, err := r.resolve("/city")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Port != 28231 {
		t.Errorf("Port = %d, want 28231", got.Port)
	}
	if got.Source != liveDoltProcessSource {
		t.Errorf("Source = %q, want %q", got.Source, liveDoltProcessSource)
	}
	want := []string{
		liveDoltHandleSource + ":not-found",
		liveDoltProcessSource + ":found",
	}
	if fmt.Sprint(attemptStatuses(got.Attempts)) != fmt.Sprint(want) {
		t.Errorf("Attempts = %v, want %v", attemptStatuses(got.Attempts), want)
	}
}

func TestLiveDoltPortResolver_ProcessTableByDataDir(t *testing.T) {
	layout := testManagedLayout()
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(layout),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{
				{PID: 2, Ports: []int{19999}, Argv: []string{"dolt", "sql-server", "--data-dir", layout.DataDir}},
			}, nil
		},
	}

	got, err := r.resolve("/city")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Port != 19999 || got.Source != liveDoltProcessSource {
		t.Errorf("got %+v, want port 19999 from %q", got, liveDoltProcessSource)
	}
}

func TestLiveDoltPortResolver_NoLiveEndpointErrors(t *testing.T) {
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(testManagedLayout()),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{
				// Listener with a foreign data dir: not ours.
				{PID: 1, Ports: []int{4000}, Argv: []string{"dolt", "sql-server", "--data-dir", "/elsewhere/dolt"}},
			}, nil
		},
	}

	got, err := r.resolve("/city")
	if !errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("err = %v, want errNoLiveDoltEndpoint", err)
	}
	if got.Port != 0 {
		t.Errorf("Port = %d, want 0", got.Port)
	}
	want := []string{
		liveDoltHandleSource + ":not-found",
		liveDoltProcessSource + ":not-found",
	}
	if fmt.Sprint(attemptStatuses(got.Attempts)) != fmt.Sprint(want) {
		t.Errorf("Attempts = %v, want %v", attemptStatuses(got.Attempts), want)
	}
}

func TestLiveDoltPortResolver_MatchingProcessWithoutPortsIsNotFound(t *testing.T) {
	layout := testManagedLayout()
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(layout),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			// Matching process that is not (yet) listening.
			return []DoltProcInfo{{PID: 2, Argv: []string{"dolt", "sql-server", "--data-dir", layout.DataDir}}}, nil
		},
	}

	_, err := r.resolve("/city")
	if !errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("err = %v, want errNoLiveDoltEndpoint", err)
	}
}

func TestLiveDoltPortResolver_AmbiguousListenersError(t *testing.T) {
	layout := testManagedLayout()
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(layout),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{
				{PID: 2, Ports: []int{28231}, Argv: []string{"dolt", "sql-server", "--data-dir", layout.DataDir}},
				{PID: 3, Ports: []int{29000}, Argv: []string{"dolt", "sql-server", "--config", layout.ConfigFile}},
			}, nil
		},
	}

	got, err := r.resolve("/city")
	if err == nil {
		t.Fatalf("resolve succeeded with ambiguous listeners: %+v", got)
	}
	if errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("ambiguity reported as no-endpoint: %v", err)
	}
	if !strings.Contains(err.Error(), "28231") || !strings.Contains(err.Error(), "29000") {
		t.Errorf("error %q does not name the candidate ports", err)
	}
	last := got.Attempts[len(got.Attempts)-1]
	if last.Source != liveDoltProcessSource || last.Status != "error" {
		t.Errorf("last attempt = %+v, want process-table error", last)
	}
}

func TestLiveDoltPortResolver_DiscoveryFailureErrors(t *testing.T) {
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "" },
		runtimeLayout:     fakeLayoutFn(testManagedLayout()),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return nil, errors.New("ps exploded")
		},
	}

	got, err := r.resolve("/city")
	if err == nil {
		t.Fatal("resolve succeeded although discovery failed")
	}
	last := got.Attempts[len(got.Attempts)-1]
	if last.Source != liveDoltProcessSource || last.Status != "error" || !strings.Contains(last.Detail, "ps exploded") {
		t.Errorf("last attempt = %+v, want process-table discovery error", last)
	}
}

func TestLiveDoltPortResolver_InvalidHandleValueFallsThrough(t *testing.T) {
	layout := testManagedLayout()
	r := liveDoltPortResolver{
		managedHandlePort: func(string) string { return "not-a-port" },
		runtimeLayout:     fakeLayoutFn(layout),
		discoverProcesses: func() ([]DoltProcInfo, error) {
			return []DoltProcInfo{{PID: 2, Ports: []int{28231}, Argv: []string{"dolt", "sql-server", "--data-dir", layout.DataDir}}}, nil
		},
	}

	got, err := r.resolve("/city")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Port != 28231 || got.Source != liveDoltProcessSource {
		t.Errorf("got %+v, want process-table fallback after invalid handle value", got)
	}
	if got.Attempts[0].Status != "error" {
		t.Errorf("handle attempt = %+v, want recorded error", got.Attempts[0])
	}
}

func TestLiveDoltPortResolver_EmptyCityPathErrors(t *testing.T) {
	r := newLiveDoltPortResolver()

	got, err := r.resolve("")
	if !errors.Is(err, errNoLiveDoltEndpoint) {
		t.Fatalf("err = %v, want errNoLiveDoltEndpoint", err)
	}
	for _, a := range got.Attempts {
		if a.Status != "not-provided" {
			t.Errorf("attempt %+v, want not-provided", a)
		}
	}
}

func TestDoltProcMatchesManagedLayout(t *testing.T) {
	layout := testManagedLayout()
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"config space form", []string{"dolt", "sql-server", "--config", layout.ConfigFile}, true},
		{"config equals form", []string{"dolt", "sql-server", "--config=" + layout.ConfigFile}, true},
		{"data-dir space form", []string{"dolt", "sql-server", "--data-dir", layout.DataDir}, true},
		{"data-dir equals form", []string{"dolt", "sql-server", "--data-dir=" + layout.DataDir}, true},
		{"foreign config", []string{"dolt", "sql-server", "--config", "/other/config.yaml"}, false},
		{"foreign data dir", []string{"dolt", "sql-server", "--data-dir", "/other/dolt"}, false},
		{"no flags", []string{"dolt", "sql-server"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doltProcMatchesManagedLayout(DoltProcInfo{Argv: tc.argv}, layout); got != tc.want {
				t.Errorf("doltProcMatchesManagedLayout(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

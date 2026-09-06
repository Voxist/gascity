package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

func runOnListOrders() []orders.Order {
	return []orders.Order{
		{Name: "merge-sweep", Trigger: "cooldown", Interval: "5m", Exec: "scripts/sweep.sh", RunOn: orders.RunOnFleetHost},
		{Name: "local-lint", Trigger: "cooldown", Interval: "1m", Exec: "scripts/lint.sh"},
	}
}

func TestOrderListShowsRunOnColumn(t *testing.T) {
	var stdout bytes.Buffer
	if code := doOrderList(runOnListOrders(), orders.RoleSeat, &stdout); code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	out := stdout.String()
	header := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	if !strings.Contains(header, "RUN_ON") {
		t.Fatalf("header = %q, want a RUN_ON column", header)
	}
	if !strings.Contains(out, orders.RunOnFleetHost) {
		t.Errorf("stdout missing the declared run_on value:\n%s", out)
	}
	// An order with no run_on renders the effective default rather than a blank.
	if !strings.Contains(out, orders.RunOnAny) {
		t.Errorf("stdout missing %q for the undeclared order:\n%s", orders.RunOnAny, out)
	}
}

func TestOrderListMarksRunOnSkippedRows(t *testing.T) {
	var stdout bytes.Buffer
	if code := doOrderList(runOnListOrders(), orders.RoleSeat, &stdout); code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		switch {
		case strings.HasPrefix(line, "merge-sweep"):
			if !strings.Contains(line, "(skipped: run_on)") {
				t.Errorf("fleet-host order on a seat not marked:\n%s", line)
			}
		case strings.HasPrefix(line, "local-lint"):
			if strings.Contains(line, "(skipped: run_on)") {
				t.Errorf("order with no run_on wrongly marked:\n%s", line)
			}
		}
	}
}

func TestOrderListRunOnUnmarkedOnFleetHost(t *testing.T) {
	var stdout bytes.Buffer
	if code := doOrderList(runOnListOrders(), orders.RoleFleetHost, &stdout); code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	if strings.Contains(stdout.String(), "(skipped: run_on)") {
		t.Fatalf("no row should be marked skipped on the fleet host:\n%s", stdout.String())
	}
}

// A city with no fleet-singleton orders keeps the table it has today.
func TestOrderListOmitsRunOnColumnWhenUndeclared(t *testing.T) {
	aa := []orders.Order{{Name: "local-lint", Trigger: "cooldown", Interval: "1m", Exec: "scripts/lint.sh"}}
	var stdout bytes.Buffer
	if code := doOrderList(aa, orders.RoleSeat, &stdout); code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	header := strings.SplitN(strings.TrimSpace(stdout.String()), "\n", 2)[0]
	if strings.Contains(header, "RUN_ON") {
		t.Fatalf("header = %q, should omit RUN_ON when no order declares it", header)
	}
}

func TestOrderListRunOnWithRigColumn(t *testing.T) {
	aa := append(runOnListOrders(), orders.Order{
		Name: "rig-probe", Trigger: "cooldown", Interval: "2m", Exec: "scripts/probe.sh", Rig: "demo-repo",
	})
	var stdout bytes.Buffer
	if code := doOrderList(aa, orders.RoleSeat, &stdout); code != 0 {
		t.Fatalf("doOrderList = %d, want 0", code)
	}
	header := strings.SplitN(strings.TrimSpace(stdout.String()), "\n", 2)[0]
	if !strings.Contains(header, "RIG") || !strings.Contains(header, "RUN_ON") {
		t.Fatalf("header = %q, want both RIG and RUN_ON", header)
	}
	if !strings.Contains(stdout.String(), "demo-repo") {
		t.Errorf("stdout missing the rig value:\n%s", stdout.String())
	}
}

func TestOrderListJSONCarriesRunOn(t *testing.T) {
	var stdout bytes.Buffer
	if code := doOrderListJSON("/city", &config.City{}, runOnListOrders(), &stdout); code != 0 {
		t.Fatalf("doOrderListJSON = %d, want 0", code)
	}
	var got struct {
		Orders []struct {
			Name  string `json:"name"`
			RunOn string `json:"run_on"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("order list JSON invalid: %v\n%s", err, stdout.String())
	}
	if got.Orders[0].RunOn != orders.RunOnFleetHost {
		t.Errorf("orders[0].run_on = %q, want %q", got.Orders[0].RunOn, orders.RunOnFleetHost)
	}
	if got.Orders[1].RunOn != "" {
		t.Errorf("orders[1].run_on = %q, want it omitted", got.Orders[1].RunOn)
	}
}

func TestOrderShowDisplaysRunOn(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := doOrderShow(runOnListOrders(), "merge-sweep", "", &stdout, &stderr); code != 0 {
		t.Fatalf("doOrderShow = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Run on:      "+orders.RunOnFleetHost) {
		t.Fatalf("stdout missing the run_on line:\n%s", stdout.String())
	}
}

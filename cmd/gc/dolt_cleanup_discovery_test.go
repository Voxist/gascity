package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestDoltProcRigOwner_MatchesDataDirUnderRigRoot(t *testing.T) {
	rigs := []resolverRig{
		{Name: "hq", Path: "/city", HQ: true},
		{Name: "alpha", Path: "/rig-a"},
	}

	proc := DoltProcInfo{Argv: []string{"dolt", "sql-server", "--data-dir", "/rig-a/.beads/dolt"}}
	owner, ok := doltProcRigOwner(proc, rigs)
	if !ok || owner != "alpha" {
		t.Errorf("doltProcRigOwner = (%q, %v), want (alpha, true)", owner, ok)
	}
}

func TestDoltProcRigOwner_MatchesConfigUnderRigRoot(t *testing.T) {
	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}

	proc := DoltProcInfo{Argv: []string{"dolt", "sql-server", "--config", "/city/.gc/runtime/packs/dolt/config.yaml"}}
	owner, ok := doltProcRigOwner(proc, rigs)
	if !ok || owner != "hq" {
		t.Errorf("doltProcRigOwner = (%q, %v), want (hq, true)", owner, ok)
	}
}

func TestDoltProcRigOwner_IgnoresForeignAndPathlessProcesses(t *testing.T) {
	rigs := []resolverRig{
		{Name: "hq", Path: "/city", HQ: true},
		{Name: "alpha", Path: "/rig-a"},
	}

	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"foreign data dir", []string{"dolt", "sql-server", "--data-dir", "/elsewhere/dolt"}},
		{"foreign config", []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"}},
		{"no path flags", []string{"dolt", "sql-server"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if owner, ok := doltProcRigOwner(DoltProcInfo{Argv: tc.argv}, rigs); ok {
				t.Errorf("doltProcRigOwner = (%q, true), want no owner", owner)
			}
		})
	}
}

func TestDoltProcRigOwner_FirstListedRigWinsOnNestedRoots(t *testing.T) {
	// Pathological: nested rig roots both contain the candidate path. The
	// first-listed rig wins; the reaper is still safe because any match
	// protects regardless of attribution.
	rigs := []resolverRig{
		{Name: "outer", Path: "/work"},
		{Name: "inner", Path: "/work/rig-a"},
	}

	proc := DoltProcInfo{Argv: []string{"dolt", "sql-server", "--data-dir", "/work/rig-a/.beads/dolt"}}
	owner, ok := doltProcRigOwner(proc, rigs)
	if !ok || owner != "outer" {
		t.Errorf("doltProcRigOwner = (%q, %v), want (outer, true)", owner, ok)
	}
}

func TestPathUnderRoot(t *testing.T) {
	cases := []struct {
		name string
		path string
		root string
		want bool
	}{
		{"direct child", "/city/.beads/dolt", "/city", true},
		{"equal", "/city", "/city", true},
		{"sibling prefix is not containment", "/cityscape/.beads", "/city", false},
		{"outside", "/elsewhere/dolt", "/city", false},
		{"empty path", "", "/city", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathUnderRoot(tc.path, normalizePathForCompare(tc.root)); got != tc.want {
				t.Errorf("pathUnderRoot(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
			}
		})
	}
}

func TestSplitCmdline_NULSeparatedWithTrailingNUL(t *testing.T) {
	// /proc/<pid>/cmdline format: NUL-separated argv, trailing NUL.
	in := []byte("dolt\x00sql-server\x00--config\x00/tmp/TestFoo/config.yaml\x00")
	got := splitCmdline(in)
	want := []string{"dolt", "sql-server", "--config", "/tmp/TestFoo/config.yaml"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d, got=%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitCmdline_Empty(t *testing.T) {
	if got := splitCmdline(nil); len(got) != 0 {
		t.Errorf("splitCmdline(nil) = %v, want empty", got)
	}
	if got := splitCmdline([]byte{}); len(got) != 0 {
		t.Errorf("splitCmdline([]) = %v, want empty", got)
	}
}

func TestParseProcStartTimeTicks(t *testing.T) {
	fieldsAfterComm := []string{
		"S", "1", "2", "3", "4", "5", "6", "7", "8", "9",
		"10", "11", "12", "13", "14", "15", "16", "17", "18", "98765",
	}
	line := "123 (dolt sql server) " + strings.Join(fieldsAfterComm, " ")

	if got := parseProcStartTimeTicks([]byte(line)); got != 98765 {
		t.Fatalf("parseProcStartTimeTicks = %d, want 98765", got)
	}
}

func TestLooksLikeDoltSQLServer(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"absolute dolt path", []string{"/usr/local/bin/dolt", "sql-server"}, true},
		{"bare dolt", []string{"dolt", "sql-server", "--config", "x"}, true},
		{"non-dolt", []string{"mysqld", "sql-server"}, false},
		{"dolt without sql-server", []string{"dolt", "version"}, false},
		{"too short", []string{"dolt"}, false},
		{"empty", []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeDoltSQLServer(tc.argv); got != tc.want {
				t.Errorf("looksLikeDoltSQLServer(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestParseDoltPSLine_DoltSQLServer(t *testing.T) {
	line := "  78306  65392 Sun May 17 09:31:24 2026 /usr/local/bin/dolt sql-server --config /tmp/TestGcBeadsBdStartUsesRootBeadsDataDir802378814/001/.gc/runtime/packs/dolt/dolt-config.yaml --host 127.0.0.1"
	got, ok := parseDoltPSLine(line, map[int][]int{78306: {3306}})
	if !ok {
		t.Fatal("parseDoltPSLine did not recognize dolt sql-server")
	}
	if got.PID != 78306 {
		t.Fatalf("PID = %d, want 78306", got.PID)
	}
	if got.RSSBytes != 65392*1024 {
		t.Fatalf("RSSBytes = %d, want %d", got.RSSBytes, int64(65392*1024))
	}
	if !reflect.DeepEqual(got.Ports, []int{3306}) {
		t.Fatalf("Ports = %v, want [3306]", got.Ports)
	}
	if got.StartIdentity != "Sun May 17 09:31:24 2026" {
		t.Fatalf("StartIdentity = %q", got.StartIdentity)
	}
	if cfg := extractConfigPath(got.Argv); cfg != "/tmp/TestGcBeadsBdStartUsesRootBeadsDataDir802378814/001/.gc/runtime/packs/dolt/dolt-config.yaml" {
		t.Fatalf("config = %q", cfg)
	}
}

func TestParseDoltPSLine_PreservesSpacedConfigPath(t *testing.T) {
	line := "12345 1024 Sun May 17 09:31:24 2026 dolt sql-server --config /tmp/Test With Space/config.yaml --port 3306"
	got, ok := parseDoltPSLine(line, nil)
	if !ok {
		t.Fatal("parseDoltPSLine did not recognize dolt sql-server")
	}
	if cfg := extractConfigPath(got.Argv); cfg != "/tmp/Test With Space/config.yaml" {
		t.Fatalf("config = %q", cfg)
	}
}

func TestParseDoltPSLine_IgnoresNonDolt(t *testing.T) {
	line := "12345 1024 Sun May 17 09:31:24 2026 mysqld --config /tmp/TestX/config.yaml"
	if got, ok := parseDoltPSLine(line, nil); ok {
		t.Fatalf("parseDoltPSLine = %+v, want ignored", got)
	}
}

func TestParseListeningPortsByPIDFromLsof(t *testing.T) {
	output := `COMMAND   PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
dolt    78306 dbox   11u  IPv4 0x0000000000000000      0t0  TCP 127.0.0.1:3306 (LISTEN)
dolt    78306 dbox   12u  IPv6 0x0000000000000000      0t0  TCP [::1]:3307 (LISTEN)
dolt    99999 dbox   12u  IPv4 0x0000000000000000      0t0  TCP 127.0.0.1:70000 (LISTEN)
`
	got := parseListeningPortsByPIDFromLsof(output)
	want := map[int][]int{78306: {3306, 3307}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseListeningPortsByPIDFromLsof = %v, want %v", got, want)
	}
}

func TestSameReapProcessIdentity_UsesPSStartIdentityFallback(t *testing.T) {
	target := ReapTarget{PID: 42, StartIdentity: "Sun May 17 09:31:24 2026"}
	same := DoltProcInfo{PID: 42, StartIdentity: "Sun May 17 09:31:24 2026"}
	reused := DoltProcInfo{PID: 42, StartIdentity: "Sun May 17 09:32:00 2026"}

	if !sameReapProcessIdentity(target, same) {
		t.Fatal("sameReapProcessIdentity should accept matching ps start identity")
	}
	if sameReapProcessIdentity(target, reused) {
		t.Fatal("sameReapProcessIdentity should reject mismatched ps start identity")
	}
}

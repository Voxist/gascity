//go:build integration

package beads

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// probeDBNamePattern constrains the probe database name to characters that are
// safe to interpolate into an SQL identifier. The name reaches the query as an
// identifier, where a placeholder cannot be used, so it is validated instead of
// escaped — a whitelist is the sound fix, quoting is not.
var probeDBNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// The identifier whitelist must actually reject the shapes that would let an
// operator-supplied database name change the meaning of the probe's queries.
// It needs no server, so it runs whenever the integration tag is on.
func TestProbeDBNamePatternRejectsSQLIdentifierInjection(t *testing.T) {
	for _, ok := range []string{"gc", "hq_scratch", "probe1", "A_1"} {
		if !probeDBNamePattern.MatchString(ok) {
			t.Errorf("probeDBNamePattern rejected the plain identifier %q", ok)
		}
	}
	for _, bad := range []string{
		"",
		"hq`.issues; drop database hq; -- ",
		"hq`",
		"hq.issues",
		"hq issues",
		"hq-issues",
		"hq\nissues",
		"hq'",
	} {
		if probeDBNamePattern.MatchString(bad) {
			t.Errorf("probeDBNamePattern accepted %q, which is not a bare SQL identifier "+
				"and would be interpolated into the probe queries verbatim", bad)
		}
	}
}

// TestNativeDoltStoreCreateWithStorageRoutesToWispsLive is the end-to-end proof
// for vp-ia76 against a REAL Dolt sql-server and a REAL beads schema — not a
// fake storage. It measures the thing the epic is about: the Dolt commit count.
//
// It is opt-in twice over: the `integration` build tag keeps it out of the
// default suite (AGENTS.md, "Integration tests use //go:build integration"),
// and it still skips unless a scratch server is configured. Run it with:
//
//	GC_NATIVE_WISP_PROBE_SCOPE=<dir containing .beads>  \
//	GC_NATIVE_WISP_PROBE_DSN='root@tcp(127.0.0.1:49991)/<db>'  \
//	GC_NATIVE_WISP_PROBE_DB=<db>  \
//	GC_NATIVE_WISP_PROBE_PORT=49991  \
//	go test -tags integration ./internal/beads/ \
//	  -run TestNativeDoltStoreCreateWithStorageRoutesToWispsLive -count=1 -v
//
// GC_NATIVE_WISP_PROBE_DB is interpolated into the probe queries as an SQL
// identifier (placeholders bind values, not identifiers), so it is validated
// against probeDBNamePattern first.
//
// The always-on sibling is TestNativeDoltStoreCreateWithStorageStampsClass,
// which pins the field routing against the in-memory storage fixture. This one
// exists for the part a fixture cannot show: the real table the row lands in
// and the Dolt commit it does or does not cost.
//
// NEVER point it at the live fleet server. It writes beads.
func TestNativeDoltStoreCreateWithStorageRoutesToWispsLive(t *testing.T) {
	scope := os.Getenv("GC_NATIVE_WISP_PROBE_SCOPE")
	dsn := os.Getenv("GC_NATIVE_WISP_PROBE_DSN")
	dbName := os.Getenv("GC_NATIVE_WISP_PROBE_DB")
	port := os.Getenv("GC_NATIVE_WISP_PROBE_PORT")
	if scope == "" || dsn == "" || dbName == "" || port == "" {
		t.Skip("scratch Dolt server not configured; see the doc comment")
	}
	if !probeDBNamePattern.MatchString(dbName) {
		t.Fatalf("GC_NATIVE_WISP_PROBE_DB = %q is not a plain [A-Za-z0-9_] identifier; "+
			"it is interpolated into the probe queries as an SQL identifier", dbName)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open probe db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	commits := func() int {
		var n int
		if err := db.QueryRow(fmt.Sprintf("select count(*) from `%s`.dolt_log", dbName)).Scan(&n); err != nil {
			t.Fatalf("count dolt_log: %v", err)
		}
		return n
	}
	tableOf := func(id string) string {
		var n int
		if err := db.QueryRow(fmt.Sprintf("select count(*) from `%s`.issues where id = ?", dbName), id).Scan(&n); err != nil {
			t.Fatalf("count issues: %v", err)
		}
		if n > 0 {
			return "issues"
		}
		if err := db.QueryRow(fmt.Sprintf("select count(*) from `%s`.wisps where id = ?", dbName), id).Scan(&n); err != nil {
			t.Fatalf("count wisps: %v", err)
		}
		if n > 0 {
			return "wisps"
		}
		return "missing"
	}

	ctx := context.Background()
	storage, err := OpenNativeStorage(ctx, scope, map[string]string{
		"BEADS_DOLT_SERVER_MODE":     "1",
		"BEADS_DOLT_SERVER_HOST":     "127.0.0.1",
		"BEADS_DOLT_SERVER_PORT":     port,
		"BEADS_DOLT_SERVER_USER":     "root",
		"BEADS_DOLT_SERVER_DATABASE": dbName,
		"BEADS_DOLT_SERVER_TLS":      "false",
		"BEADS_DOLT_AUTO_START":      "0",
	})
	if err != nil {
		t.Fatalf("OpenNativeStorage: %v", err)
	}
	defer storage.Close() //nolint:errcheck
	store := newNativeDoltStoreForTest(storage)

	sessionBead := func(title string) Bead {
		return Bead{Title: title, Type: "session", Labels: []string{"gc:session", "agent:probe"}}
	}

	// Control: the current behavior. A session bead created without a storage
	// class lands in the committed issues table and costs one Dolt commit.
	before := commits()
	plain, err := store.Create(sessionBead("probe plain session"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plainCost := commits() - before
	if got := tableOf(plain.ID); got != "issues" {
		t.Fatalf("plain session create landed in %s, want issues", got)
	}
	if plainCost != 1 {
		t.Fatalf("plain session create cost %d Dolt commits, want 1", plainCost)
	}

	// The fix: the same bead with the session policy's storage class must land
	// in the dolt_ignore'd wisps table at zero commits.
	before = commits()
	wisp, err := store.CreateWithStorage(sessionBead("probe no-history session"), StorageNoHistory)
	if err != nil {
		t.Fatalf("CreateWithStorage: %v", err)
	}
	wispCost := commits() - before
	if got := tableOf(wisp.ID); got != "wisps" {
		t.Fatalf("no-history session create landed in %s, want wisps", got)
	}
	if wispCost != 0 {
		t.Fatalf("no-history session create cost %d Dolt commits, want 0", wispCost)
	}

	// The payload claim: updates are where most of hq's commit graph comes from
	// (2,885 of 3,363 commits in a measured 6h window), so measure them, not
	// only the create.
	before = commits()
	status := "in_progress"
	for i := 0; i < 5; i++ {
		if err := store.Update(plain.ID, UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update issues-table session: %v", err)
		}
		open := "open"
		if err := store.Update(plain.ID, UpdateOpts{Status: &open}); err != nil {
			t.Fatalf("Update issues-table session: %v", err)
		}
	}
	issuesUpdateCost := commits() - before

	before = commits()
	for i := 0; i < 5; i++ {
		if err := store.Update(wisp.ID, UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update wisp session: %v", err)
		}
		open := "open"
		if err := store.Update(wisp.ID, UpdateOpts{Status: &open}); err != nil {
			t.Fatalf("Update wisp session: %v", err)
		}
	}
	wispUpdateCost := commits() - before

	t.Logf("10 updates: issues-table session = %d commits, wisps-table session = %d commits",
		issuesUpdateCost, wispUpdateCost)
	if wispUpdateCost != 0 {
		t.Fatalf("10 updates to a wisps-table session cost %d Dolt commits, want 0", wispUpdateCost)
	}
	if issuesUpdateCost == 0 {
		t.Fatal("10 updates to an issues-table session cost 0 Dolt commits: " +
			"the control is not exercising the committing path, so the comparison proves nothing")
	}
}

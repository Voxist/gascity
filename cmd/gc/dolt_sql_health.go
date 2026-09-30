package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/doltpool"
)

type managedDoltSQLHealthReport struct {
	QueryReady      bool
	ReadOnly        string
	ConnectionCount string
}

// managedDoltProbeDatabase is the legacy dedicated probe database name. The
// read-only probe no longer creates or writes to it: Dolt's autostats subsystem
// (statspro) randomly elects one server-wide database to host the on-disk
// stats backing store, and a tiny dedicated DB lost the lottery in production
// by accumulating stats noms it was never meant to hold. The probe now writes
// into a GC-owned table inside a discovered user database instead so it shares
// a backing store with real workload traffic. This constant remains so
// `gc dolt-state reset-probe` can still drop the legacy DB on demand and so
// `gc dolt-state init` can keep rejecting it as a user-supplied database name.
const managedDoltProbeDatabase = "__gc_probe"

const managedDoltProbeTable = "__gc_read_only_probe"

var errManagedDoltNoUserDatabase = errors.New("no user database available for managed Dolt read-only probe")

// errManagedDoltQueryProbeTimeout marks a query-probe failure that
// managedDoltSQLCommandTimeout (or, on the direct SQL-driver lane, its own
// equivalent 5s context) caused, as distinct from a probe that ran to
// completion and answered bad.
//
// ga-z3c6p: this is the query-probe half of the ga-amol9 class of bug.
// op_health's tcp_check already had this distinction (die_unobservable vs
// die); the query probe that runs after it did not, so a probe that timed
// out under exactly the host load that starves tcp_check's own `nc` was
// read as "the server answered and is bad" (managedDoltHealthOpEvidence ->
// recoverEvidenceHealthOpAnswered -> mustProveDeath()=false -> liveness
// skipped, dolt_recover_gate.go) rather than "could not observe" —
// authorizing a live-server replacement on nothing but slowness. `gc
// dolt-state health-check`/`query-probe` (cmd_dolt_state.go) check for this
// sentinel and exit with providerOpExitUnobservable (3) instead of the
// generic errExit (1) so gc-beads-bd.sh's op_health can call
// die_unobservable instead of die.
var errManagedDoltQueryProbeTimeout = errors.New("managed dolt query probe timed out")

// errManagedDoltSQLCommandTimeout marks a runManagedDoltSQLContext failure
// caused by hitting managedDoltSQLCommandTimeout, as distinct from the
// forked `dolt sql` CLI running to completion and reporting its own error.
// Shared by every caller of runManagedDoltSQL(Context) (query probe,
// read-only state, connection count, database listing); only the query
// probe currently acts on it (see errManagedDoltQueryProbeTimeout above),
// but the distinction is real for any of them.
var errManagedDoltSQLCommandTimeout = errors.New("managed dolt sql command timed out")

var (
	managedDoltQueryProbeDirectFn      = managedDoltQueryProbeDirect
	managedDoltReadOnlyStateDirectFn   = managedDoltReadOnlyStateDirect
	managedDoltConnectionCountDirectFn = managedDoltConnectionCountDirect
	managedDoltResetProbeDirectFn      = managedDoltResetProbeDirect
	managedDoltSQLCommandTimeout       = 5 * time.Second
)

// managedDoltSystemDatabases lists databases that the read-only probe must not
// pick as its write target. `__gc_probe` is included so existing legacy data
// is left in place while we migrate off of it.
var managedDoltSystemDatabases = map[string]struct{}{
	"information_schema":     {},
	"mysql":                  {},
	"dolt":                   {},
	"dolt_cluster":           {},
	"performance_schema":     {},
	"sys":                    {},
	managedDoltProbeDatabase: {},
}

// managedDoltReadOnlyProbeStatementsFor returns the read-only probe statements
// for db. Each invocation creates the persistent GC-owned probe table inside db
// (idempotent), rewrites a single row to test writability, and registers the
// table in dolt_ignore so history flattening can never first-commit it: a
// non-ignored table that lives only in the working set is committed by the
// compaction flatten's `DOLT_COMMIT -Am`, which drifts the database hash and
// quarantines GC for that database (hq June 2026, daa 2026-08-04). The
// registration is last so a read-only server still fails on the CREATE or
// REPLACE, which is what the read-only classification keys on, and it uses
// INSERT IGNORE so an operator's explicit `ignored = 0` override survives.
// The probe opens with USE because dolt_ignore is a session-root-backed system
// table: both probe paths (the dolt CLI over --host and the direct driver)
// connect with no default schema, and writing dolt_ignore through a qualified
// name on such a session fails with "no root value found in session" — USE is
// read-only, so the read-only classification still keys on the CREATE/REPLACE.
// db must be a real user database; the empty string returns nil so the caller
// can skip the probe entirely. The database identifier is backtick-quoted
// because Dolt derives DB names from repository directory names, which can start
// with a digit or contain other characters that need quoting.
func managedDoltReadOnlyProbeStatementsFor(db string) []string {
	db = strings.TrimSpace(db)
	if db == "" {
		return nil
	}
	quotedDB := managedDoltQuoteIdent(db)
	target := quotedDB + "." + managedDoltQuoteIdent(managedDoltProbeTable)
	return []string{
		"USE " + quotedDB,
		"CREATE TABLE IF NOT EXISTS " + target + " (k INT PRIMARY KEY)",
		"REPLACE INTO " + target + " VALUES (1)",
		"INSERT IGNORE INTO " + quotedDB + ".`dolt_ignore` (pattern, ignored) VALUES ('" + managedDoltProbeTable + "', 1)",
	}
}

// managedDoltQuoteIdent backtick-quotes a SQL identifier and escapes any
// embedded backticks by doubling them (MySQL convention).
func managedDoltQuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// managedDoltReadOnlyProbeSQLFor joins managedDoltReadOnlyProbeStatementsFor
// into a single semicolon-terminated SQL string suitable for passing to
// `dolt sql -q`.
func managedDoltReadOnlyProbeSQLFor(db string) string {
	stmts := managedDoltReadOnlyProbeStatementsFor(db)
	if len(stmts) == 0 {
		return ""
	}
	return strings.Join(stmts, "; ") + ";"
}

func managedDoltQueryProbe(host, port, user string) error {
	if managedDoltPassword() != "" {
		return managedDoltQueryProbeDirectFn(host, port, user)
	}
	_, err := runManagedDoltSQL(host, port, user, "-r", "csv", "-q", "SELECT COUNT(*) AS cnt FROM information_schema.SCHEMATA")
	if err == nil {
		return nil
	}
	if errors.Is(err, errManagedDoltSQLCommandTimeout) {
		return fmt.Errorf("%w: %w", errManagedDoltQueryProbeTimeout, err)
	}
	if strings.TrimSpace(err.Error()) == "" {
		return fmt.Errorf("query probe failed")
	}
	return err
}

func managedDoltReadOnlyState(host, port, user string) (string, error) {
	if managedDoltPassword() != "" {
		return managedDoltReadOnlyStateDirectFn(host, port, user)
	}
	db, err := managedDoltSelectUserDatabase(host, port, user)
	if err != nil {
		return "unknown", err
	}
	if db == "" {
		return "unknown", errManagedDoltNoUserDatabase
	}
	_, err = runManagedDoltSQL(host, port, user, "-q", managedDoltReadOnlyProbeSQLFor(db))
	if err == nil {
		return "false", nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "read only") || strings.Contains(msg, "read-only") {
		return "true", nil
	}
	return "unknown", err
}

// managedDoltSelectUserDatabase returns the first database from SHOW DATABASES
// that is not a system database. It returns "" when the server has no user database.
func managedDoltSelectUserDatabase(host, port, user string) (string, error) {
	dbs, err := managedDoltSelectUserDatabases(host, port, user)
	if err != nil || len(dbs) == 0 {
		return "", err
	}
	return dbs[0], nil
}

func managedDoltSelectUserDatabases(host, port, user string) ([]string, error) {
	out, err := runManagedDoltSQL(host, port, user, "-r", "csv", "-q", "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	return managedDoltUserDatabasesFromCSV(out)
}

// managedDoltFirstUserDatabaseFromCSV parses csv-format `SHOW DATABASES`
// output and returns the first non-system database, or "" when none exist.
func managedDoltFirstUserDatabaseFromCSV(out string) (string, error) {
	dbs, err := managedDoltUserDatabasesFromCSV(out)
	if err != nil || len(dbs) == 0 {
		return "", err
	}
	return dbs[0], nil
}

func managedDoltUserDatabasesFromCSV(out string) ([]string, error) {
	reader := csv.NewReader(strings.NewReader(out))
	reader.FieldsPerRecord = 1
	dbs := []string{}
	for {
		record, err := reader.Read()
		if err == io.EOF {
			return dbs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse SHOW DATABASES csv: %w", err)
		}
		dbs = append(dbs, managedDoltUserDatabases(record)...)
	}
}

// managedDoltFirstUserDatabase scans database names and returns the first non-system
// database, or "" when none exist.
func managedDoltFirstUserDatabase(lines []string) string {
	dbs := managedDoltUserDatabases(lines)
	if len(dbs) == 0 {
		return ""
	}
	return dbs[0]
}

func managedDoltUserDatabases(lines []string) []string {
	dbs := []string{}
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, "Database") {
			continue
		}
		if _, system := managedDoltSystemDatabases[strings.ToLower(name)]; system {
			continue
		}
		dbs = append(dbs, name)
	}
	return dbs
}

func managedDoltConnectionCount(host, port, user string) (string, error) {
	if managedDoltPassword() != "" {
		return managedDoltConnectionCountDirectFn(host, port, user)
	}
	out, err := runManagedDoltSQL(host, port, user, "-r", "csv", "-q", "SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST")
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		_, parseErr := strconv.Atoi(line)
		if parseErr == nil {
			return line, nil
		}
		return "", fmt.Errorf("parse connection count %q: %w", line, parseErr)
	}
	return "", fmt.Errorf("parse connection count from %q", strings.TrimSpace(out))
}

func managedDoltHealthCheck(host, port, user string, checkReadOnly bool) (managedDoltSQLHealthReport, error) {
	if err := managedDoltQueryProbe(host, port, user); err != nil {
		return managedDoltSQLHealthReport{}, err
	}
	report := managedDoltSQLHealthReport{
		QueryReady: true,
		ReadOnly:   "false",
	}
	if checkReadOnly {
		// The query probe above already proved the server answers (that is
		// what checkReadOnly runs after), so a read-only step that times out
		// is inconclusive, not an observation that the server is bad — it
		// must not turn an otherwise-healthy check into a failure any more
		// than "no user database to probe" does. errManagedDoltSQLCommandTimeout
		// covers both lanes: the CLI lane's runManagedDoltSQLContext wraps its
		// own timeouts with it directly, and managedDoltReadOnlyStateDirect
		// wraps its own via managedDoltWrapSQLCommandTimeout for the same
		// reason (ga-z3c6p).
		state, err := managedDoltReadOnlyState(host, port, user)
		if err != nil {
			if !errors.Is(err, errManagedDoltNoUserDatabase) && !errors.Is(err, errManagedDoltSQLCommandTimeout) {
				return managedDoltSQLHealthReport{}, err
			}
		}
		report.ReadOnly = state
	}
	if count, err := managedDoltConnectionCount(host, port, user); err == nil {
		report.ConnectionCount = count
	}
	return report, nil
}

func managedDoltHealthCheckFields(report managedDoltSQLHealthReport) []string {
	if !report.QueryReady {
		return []string{"query_ready\tfalse"}
	}
	return []string{
		"query_ready\ttrue",
		"read_only\t" + report.ReadOnly,
		"connection_count\t" + report.ConnectionCount,
	}
}

func managedDoltPassword() string {
	return strings.TrimSpace(os.Getenv("GC_DOLT_PASSWORD"))
}

// managedDoltOpenDB returns the shared pooled server-level *sql.DB (no
// database selected) for a managed Dolt endpoint. The handle is owned by
// internal/doltpool — callers must NOT Close it.
func managedDoltOpenDB(host, port, user string) (*sql.DB, error) {
	host = managedDoltConnectHost(host)
	port = strings.TrimSpace(port)
	if port == "" {
		return nil, fmt.Errorf("missing port")
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	return doltpool.Open(host, port, user, managedDoltPassword(), "")
}

func managedDoltQueryProbeDirect(host, port, user string) error {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return managedDoltWrapQueryProbeTimeout(ctx, err)
	}
	var cnt int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) AS cnt FROM information_schema.SCHEMATA").Scan(&cnt); err != nil {
		return managedDoltWrapQueryProbeTimeout(ctx, err)
	}
	return nil
}

// managedDoltWrapQueryProbeTimeout marks err with errManagedDoltQueryProbeTimeout
// when ctx's deadline is what actually ended the call (see
// errManagedDoltQueryProbeTimeout's doc comment) — mirroring
// runManagedDoltSQLContext's own ctx.Err()-gated classification on the
// CLI-fork lane, so both lanes of managedDoltQueryProbe produce the same
// signal.
func managedDoltWrapQueryProbeTimeout(ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%w: %w", errManagedDoltQueryProbeTimeout, err)
	}
	return err
}

// managedDoltWrapSQLCommandTimeout marks err with errManagedDoltSQLCommandTimeout
// when ctx's deadline is what actually ended the call, mirroring
// managedDoltWrapQueryProbeTimeout's mechanism for the query probe's direct
// lane. Applied to managedDoltReadOnlyStateDirect's failure points so a
// read-only-step timeout classifies identically to the CLI lane's
// runManagedDoltSQLContext (which already wraps its own timeouts this way) —
// letting managedDoltHealthCheck treat a read-only-step timeout the same
// regardless of lane (ga-z3c6p): the query probe already proved the server
// answers, so a later read-only-step timeout is inconclusive, not an
// observed-bad-server verdict.
func managedDoltWrapSQLCommandTimeout(ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%w: %w", errManagedDoltSQLCommandTimeout, err)
	}
	return err
}

func managedDoltReadOnlyStateDirect(host, port, user string) (string, error) {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return "unknown", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return "unknown", managedDoltWrapSQLCommandTimeout(ctx, err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return "unknown", managedDoltWrapSQLCommandTimeout(ctx, err)
	}
	defer conn.Close() //nolint:errcheck

	userDB, err := managedDoltSelectUserDatabaseFromConn(ctx, conn)
	if err != nil {
		return "unknown", managedDoltWrapSQLCommandTimeout(ctx, err)
	}
	if userDB == "" {
		return "unknown", errManagedDoltNoUserDatabase
	}
	for _, query := range managedDoltReadOnlyProbeStatementsFor(userDB) {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "read only") || strings.Contains(msg, "read-only") {
				return "true", nil
			}
			return "unknown", managedDoltWrapSQLCommandTimeout(ctx, err)
		}
	}
	return "false", nil
}

func managedDoltSelectUserDatabaseFromConn(ctx context.Context, conn *sql.Conn) (string, error) {
	dbs, err := managedDoltSelectUserDatabasesFromConn(ctx, conn)
	if err != nil || len(dbs) == 0 {
		return "", err
	}
	return dbs[0], nil
}

func managedDoltSelectUserDatabasesFromConn(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return managedDoltUserDatabases(names), nil
}

func managedDoltConnectionCountDirect(host, port, user string) (string, error) {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return "", err
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST").Scan(&count); err != nil {
		return "", err
	}
	return strconv.Itoa(count), nil
}

func managedDoltResetProbe(host, port, user string) error {
	if managedDoltPassword() != "" {
		return managedDoltResetProbeDirectFn(host, port, user)
	}
	if _, err := runManagedDoltSQL(host, port, user, "-q", "DROP DATABASE IF EXISTS "+managedDoltProbeDatabase); err != nil {
		return err
	}
	dbs, err := managedDoltSelectUserDatabases(host, port, user)
	if err != nil {
		return err
	}
	for _, db := range dbs {
		if _, err := runManagedDoltSQL(host, port, user, "-q", managedDoltDropProbeTableSQLFor(db)); err != nil {
			return err
		}
	}
	return nil
}

func managedDoltResetProbeDirect(host, port, user string) error {
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+managedDoltProbeDatabase); err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck
	dbs, err := managedDoltSelectUserDatabasesFromConn(ctx, conn)
	if err != nil {
		return err
	}
	for _, userDB := range dbs {
		if _, err := conn.ExecContext(ctx, managedDoltDropProbeTableSQLFor(userDB)); err != nil {
			return err
		}
	}
	return nil
}

func managedDoltDropProbeTableSQLFor(db string) string {
	return "DROP TABLE IF EXISTS " + managedDoltQuoteIdent(db) + "." + managedDoltQuoteIdent(managedDoltProbeTable)
}

func runManagedDoltSQL(host, port, user string, args ...string) (string, error) {
	return runManagedDoltSQLContext(context.Background(), host, port, user, args...)
}

// runManagedDoltSQLContext is the context-aware form of runManagedDoltSQL: the
// command is bounded by both managedDoltSQLCommandTimeout and the caller's ctx,
// so a hung server cannot outlive a canceled reconcile tick.
func runManagedDoltSQLContext(parent context.Context, host, port, user string, args ...string) (string, error) {
	host = managedDoltConnectHost(host)
	port = strings.TrimSpace(port)
	if port == "" {
		return "", fmt.Errorf("missing port")
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	baseArgs := []string{
		"--host", host,
		"--port", port,
		"--user", user,
		"--password", managedDoltPassword(),
		"--no-tls",
		"sql",
	}
	ctx, cancel := context.WithTimeout(parent, managedDoltSQLCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dolt", append(baseArgs, args...)...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("%w: timed out after %s: %s", errManagedDoltSQLCommandTimeout, managedDoltSQLCommandTimeout, msg)
		}
		return "", fmt.Errorf("%w: timed out after %s", errManagedDoltSQLCommandTimeout, managedDoltSQLCommandTimeout)
	}
	if err == nil {
		return string(out), nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return "", err
	}
	return "", fmt.Errorf("%s", msg)
}

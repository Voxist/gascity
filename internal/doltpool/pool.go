// Package doltpool provides a shared, process-lifetime *sql.DB registry
// for Go-native Dolt (MySQL-protocol) connections. Each distinct
// (host, port, user, password, database) combination is opened once and
// reused across callers, eliminating the per-operation Open+Close
// pattern that produces TIME_WAIT churn (2,618 sockets observed from
// one call site) and unbounded backend connections.
//
// database/sql's *sql.DB is itself a connection pool: Open here is lazy
// (no dial), connections are created on demand and bounded by the
// per-endpoint caps below. Callers must NEVER Close a returned handle;
// call Shutdown once on process exit if orderly cleanup is wanted.
//
// Ported from the vp-kxbh worktree skeleton (city-scale plan item 1.2).
package doltpool

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// Per-endpoint connection caps. With S scopes a city consumes at most
// S×maxOpenConns backend connections from gc's Go-native paths, which the
// supervisor doctor budget (≤0.8×@@max_connections, plan item 2.7) can
// reason about.
const (
	maxOpenConns = 5
	maxIdleConns = 2
	// connMaxLifetime bounds a connection's TOTAL age (busy or idle). Lowered
	// from time.Hour (vc-wz5) to a hygiene backstop that periodically recycles
	// even continuously-busy conns. The death-match guard is connMaxIdleTime
	// below, not this value — a busy conn is never idle long enough for the
	// server to kill it.
	connMaxLifetime = 20 * time.Second
	// connMaxIdleTime reaps an IDLE pooled connection client-side before the
	// managed Dolt server's read_timeout_millis kills it server-side. This is
	// THE fix for the vc-wz5 "read-timeout death match": previously the pool
	// only bounded total lifetime (1h) with no idle reaping, so an idle conn
	// unused >= the server read_timeout was closed by the server while the
	// client still trusted it for up to an hour — the driver then handed the
	// dead conn to the next op ("closing bad idle connection: EOF / reset /
	// broken pipe"), taxing every op and losing the dispatcher's last-fired
	// write under churn (town-wide order staleness).
	//
	// INVARIANT (enforced at runtime by the dolt-timeout-race doctor check):
	// this MUST stay strictly below the managed server read_timeout_millis.
	// The managed default is config.DefaultDoltReadTimeoutMillis = 15000ms; the
	// live city runs 30000ms. 10s clears both with margin. NOTE: the client
	// per-query readTimeout (below, 30s) is deliberately NOT the guard — it is
	// the response-read deadline for a query in flight, unrelated to idle-conn
	// reaping; lowering it would abort legitimately slow queries.
	connMaxIdleTime = 10 * time.Second
	connTimeout     = 5 * time.Second
	readTimeout     = 30 * time.Second
	writeTimeout    = 30 * time.Second
)

var registry = &poolRegistry{
	dbs: make(map[string]*sql.DB),
}

type poolRegistry struct {
	mu  sync.Mutex
	dbs map[string]*sql.DB
}

// key includes the password so a credential rotation (e.g. a managed
// server restart republishing auth) yields a fresh pool instead of
// serving stale-credential connections. Keys live only in process
// memory and are never logged.
func key(host, port, user, password, database string) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", user, host, port, database, password)
}

// Open returns the shared *sql.DB for the given Dolt endpoint, creating
// it on first use. database may be empty for server-level connections
// (SHOW DATABASES, health probes). The returned handle must never be
// closed by the caller; call Shutdown on process exit.
func Open(host, port, user, password, database string) (*sql.DB, error) {
	k := key(host, port, user, password, database)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if db, ok := registry.dbs[k]; ok {
		return db, nil
	}
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = host + ":" + port
	cfg.DBName = database
	cfg.Timeout = connTimeout
	cfg.ReadTimeout = readTimeout
	cfg.WriteTimeout = writeTimeout
	cfg.AllowNativePasswords = true
	// DATETIME columns scan into time.Time (the convoy workflow snapshot
	// reads created_at/updated_at this way); string scans still work.
	cfg.ParseTime = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("opening pooled dolt connection to %s:%s/%s: %w", host, port, database, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	// Idle reaping is the death-match guard (vc-wz5): reap idle conns before the
	// managed Dolt server read_timeout closes them under the client. See the
	// connMaxIdleTime doc comment and the dolt-timeout-race doctor check.
	db.SetConnMaxIdleTime(connMaxIdleTime)
	registry.dbs[k] = db
	return db, nil
}

// IdleConnCeiling reports the longest an idle pooled connection may live before
// this pool reaps it client-side: the smaller positive bound of connMaxIdleTime
// and connMaxLifetime (a non-positive bound means "no limit from that knob").
// A return of 0 means no client-side idle reaping at all — the pre-vc-wz5
// death-match configuration.
//
// The managed Dolt server's read_timeout_millis MUST exceed this value; if it
// does not, the server closes idle connections the client still trusts. The
// dolt-timeout-race doctor check asserts server read_timeout > IdleConnCeiling()
// at runtime, and this accessor is the single source of truth both the pool and
// that check read.
func IdleConnCeiling() time.Duration {
	ceiling := time.Duration(0)
	for _, bound := range []time.Duration{connMaxIdleTime, connMaxLifetime} {
		if bound <= 0 {
			continue // this knob imposes no limit
		}
		if ceiling == 0 || bound < ceiling {
			ceiling = bound
		}
	}
	return ceiling
}

// Shutdown closes all pooled connections and empties the registry. Call
// once on process exit; subsequent Open calls recreate pools on demand.
func Shutdown() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for k, db := range registry.dbs {
		db.Close() //nolint:errcheck // best-effort close on shutdown
		delete(registry.dbs, k)
	}
}

// Len returns the number of distinct endpoint entries in the registry.
func Len() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.dbs)
}

// TotalOpenConns returns the sum of open connections across all pooled
// *sql.DB instances. Use this for observability gauges; it does not
// imply pool health.
func TotalOpenConns() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	total := 0
	for _, db := range registry.dbs {
		total += db.Stats().OpenConnections
	}
	return total
}

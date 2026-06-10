// Package doltpool provides a process-lifetime *sql.DB registry for Dolt
// MySQL-wire connections. A single shared *sql.DB is returned for each unique
// (user, host, port, database) tuple; callers must never Close the returned DB.
// Call Shutdown once at process exit to drain all pools.
package doltpool

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

const (
	maxOpenConns    = 5
	maxIdleConns    = 2
	connMaxLifetime = time.Hour
)

// Config holds the connection parameters for a Dolt SQL server.
type Config struct {
	User     string
	Password string
	Host     string
	Port     int
	Database string
}

func (c Config) key() string {
	user := c.User
	if user == "" {
		user = "root"
	}
	return fmt.Sprintf("%s@tcp(%s:%d)/%s", user, c.Host, c.Port, c.Database)
}

var (
	mu       sync.Mutex
	registry = map[string]*sql.DB{}
)

// Open returns the shared *sql.DB for the given config, creating it on first
// call. The returned DB must not be closed by the caller.
func Open(cfg Config) (*sql.DB, error) {
	k := cfg.key()
	mu.Lock()
	defer mu.Unlock()
	if db, ok := registry[k]; ok {
		return db, nil
	}
	mc := mysql.NewConfig()
	mc.User = cfg.User
	if mc.User == "" {
		mc.User = "root"
	}
	mc.Passwd = cfg.Password
	mc.Net = "tcp"
	mc.Addr = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	mc.DBName = cfg.Database
	mc.AllowNativePasswords = true
	mc.ParseTime = true
	mc.Timeout = 10 * time.Second
	mc.ReadTimeout = 30 * time.Second
	mc.WriteTimeout = 30 * time.Second
	db, err := sql.Open("mysql", mc.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	registry[k] = db
	return db, nil
}

// Shutdown closes all pooled *sql.DB instances and resets the registry.
// It should be called once at process exit.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	for k, db := range registry {
		_ = db.Close()
		delete(registry, k)
	}
}

// TotalOpenConns returns the total number of open connections across all
// pooled databases. Useful for health reporting.
func TotalOpenConns() int {
	mu.Lock()
	defer mu.Unlock()
	total := 0
	for _, db := range registry {
		total += db.Stats().OpenConnections
	}
	return total
}

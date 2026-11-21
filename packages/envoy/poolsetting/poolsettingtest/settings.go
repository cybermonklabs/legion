// Package poolsettingtest holds the tests a store whose pool has a fixed default runs against that
// store's Open: that the pool holds the default whatever machine it runs on (SameOnEveryMachine),
// and that a connection string which names pool_max_conns keeps its own size, in each form pgx
// reads, while one that names none gets the default (OutranksTheDefault). Each store's own test
// opens its store and names its default; what is the same for every store is here once.
//
// It is a public package, as poolsetting is, because the daemon's store (packages/daemon/
// internal/store) runs these tests too and Go's internal rule keeps the daemon's module out of
// this module's internal tree. It links no container runtime, so a test binary that imports it
// pays no testcontainers-go init at the starts SameOnEveryMachine makes.
package poolsettingtest

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/poolsetting"
)

// ServiceName is the service ServiceFile writes, and serviceSize the pool size it names.
const (
	ServiceName = "poolsize"
	serviceSize = 7
)

// ServiceFile writes a pg_service.conf whose service ServiceName names pool_max_conns=serviceSize,
// and points PGSERVICEFILE at it for t.
func ServiceFile(t testing.TB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pg_service.conf")
	content := "[" + ServiceName + "]\n" + poolsetting.Key + "=" + strconv.Itoa(serviceSize) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", path)
}

// withSetting is connString with one more connection setting, in connString's own form: a URL's
// query, or the next quoted pair of a keyword/value string.
func withSetting(t testing.TB, connString, key, value string) string {
	t.Helper()
	if strings.HasPrefix(connString, "postgres://") || strings.HasPrefix(connString, "postgresql://") {
		u, err := url.Parse(connString)
		if err != nil {
			t.Fatalf("parse the connection URL: %v", err)
		}
		query := u.Query()
		query.Set(key, value)
		u.RawQuery = query.Encode()
		return u.String()
	}
	return connString + " " + key + "=" + quoted(value)
}

// quoted is value as a keyword/value string writes it: in single quotes, with each backslash and
// single quote escaped.
func quoted(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

// asKeywords is the connection connString names, as a keyword/value string, whichever form
// connString has: so a test can write a setting no URL carries the same way, a quoted value. A
// connection pgx would make without TLS keeps sslmode=disable.
func asKeywords(t testing.TB, connString string) string {
	t.Helper()
	config, err := pgconn.ParseConfig(connString)
	if err != nil {
		t.Fatalf("parse the connection string: %v", err)
	}
	pairs := []string{
		"host=" + quoted(config.Host), "port=" + strconv.Itoa(int(config.Port)), "user=" + quoted(config.User),
		"password=" + quoted(config.Password), "dbname=" + quoted(config.Database),
	}
	if config.TLSConfig == nil {
		pairs = append(pairs, "sslmode=disable")
	}
	for key, value := range config.RuntimeParams {
		pairs = append(pairs, key+"="+quoted(value))
	}
	return strings.Join(pairs, " ")
}

// OutranksTheDefault checks that the pool a store opens holds the size its connection string names,
// as pgx reads it: in the URL or string database is given as, in a keyword/value string, and in
// the service file a `service=` setting names. Text inside a quoted value names nothing, and a
// string that names no size gets want, the store's default. database is a reachable database;
// open opens the store on a connection string and answers its pool's MaxConns, closing the store.
func OutranksTheDefault(t *testing.T, database string, want int32, open func(t *testing.T, connString string) int32) {
	t.Helper()
	ServiceFile(t)
	keywords := asKeywords(t, database)
	for _, tc := range []struct {
		name, connString string
		want             int32
	}{
		{"the database as given, naming no size", database, want},
		{"the database as given, naming 7", withSetting(t, database, poolsetting.Key, "7"), 7},
		{"a keyword/value string naming 7", withSetting(t, keywords, poolsetting.Key, "7"), 7},
		{"the key inside a quoted value", withSetting(t, keywords, "application_name", "worker "+poolsetting.Key+"=7"), want},
		{"a service whose file names 7", withSetting(t, keywords, "service", ServiceName), serviceSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := open(t, tc.connString); got != tc.want {
				t.Errorf("Open's pool holds %d connections, want %d", got, tc.want)
			}
		})
	}
}

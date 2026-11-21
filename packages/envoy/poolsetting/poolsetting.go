// Package poolsetting reads whether a Postgres connection string names pgxpool's pool size,
// pool_max_conns. pgxpool.ParseConfig takes the key out of the config it returns, so that config
// cannot tell a named size from pgxpool's default, max(4, runtime.NumCPU()), which follows the
// machine. A store that sets its own pool size asks here first.
//
// Three stores read it, each with its own policy. The daemon's store (packages/daemon/internal/
// store) and the broker's (internal/broker/store) keep a named size and set their own default
// otherwise. Dispatch's (internal/dispatch/store) refuses a named size, because its pool is fixed
// in code. It is a public package, as migrationid is, because Go's internal rule keeps the
// daemon's module out of this module's internal tree.
package poolsetting

import "github.com/jackc/pgx/v5"

// Key is the connection setting pgxpool.ParseConfig reads a pool's MaxConns from.
const Key = "pool_max_conns"

// Lookup reports whether connString's effective settings name Key, as pgxpool itself reads them.
// pgx.ParseConfig, the parse pgxpool.ParseConfig makes first, resolves a URL's query, a
// keyword/value string with its quoting and escapes, and the keys of the service file entry a
// `service=` setting names, and keeps every key neither it nor pgconn knows among RuntimeParams,
// where pgxpool then finds Key. The raw string answers a different question: a quoted or escaped
// value may hold the key's own name, and a keyword may stand apart from its "=". It returns pgx's
// error for a connection string pgx cannot parse, which pgxpool.ParseConfig would refuse too.
func Lookup(connString string) (bool, error) {
	config, err := pgx.ParseConfig(connString)
	if err != nil {
		return false, err
	}
	_, named := config.RuntimeParams[Key]
	return named, nil
}

// Named is Lookup for a caller that parses connString itself with pgxpool.ParseConfig and reports
// that parse's error: a connection string pgx cannot parse names nothing here.
func Named(connString string) bool {
	named, _ := Lookup(connString)
	return named
}

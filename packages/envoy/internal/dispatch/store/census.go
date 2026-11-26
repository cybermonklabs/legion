package store

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/redact"
)

// censusApplicationName is how the census's one connection appears in pg_stat_activity.
const censusApplicationName = "envoy-dispatch census"

// Census takes the pre-deploy census (pgmigrate.Census) of the database databaseURL names against
// the migrations this binary embeds, on one connection of its own: a deployment runs it before it
// rolls the service, so it uses no pool and migrates nothing. A URL pgx cannot read is refused in
// fixed words, as Connect refuses one.
func Census(ctx context.Context, databaseURL string) (*pgmigrate.Report, error) {
	return census(ctx, databaseURL, migrationFiles, pgmigrate.CensusOptions{})
}

func census(ctx context.Context, databaseURL string, fsys fs.FS, options pgmigrate.CensusOptions) (*pgmigrate.Report, error) {
	migrations, err := pgmigrate.Load(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrations refused: %w", err)
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Postgres URL: %w", redact.Masked(err, "pgx cannot read its connection settings"))
	}
	config.RuntimeParams["application_name"] = censusApplicationName
	// The census's one author-written statement is pinned to the extended protocol on its own
	// call (pgmigrate.Census); this pins the connection's default too, so a connection string
	// asking for the simple protocol changes nothing the census runs.
	config.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)
	return pgmigrate.Census(ctx, conn, migrations, "schema_migrations", options)
}

// CheckVersions reads what schema_migrations, on the database databaseURL names, records of each
// version against the migrations this binary embeds, and what the runner's next boot would do
// with each row (pgmigrate.CheckVersions). It holds one connection of its own, in a read-only
// transaction, and writes nothing. A URL pgx cannot read is refused in fixed words, as Connect
// refuses one.
func CheckVersions(ctx context.Context, databaseURL string) (*pgmigrate.VersionCheck, error) {
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrations refused: %w", err)
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Postgres URL: %w", redact.Masked(err, "pgx cannot read its connection settings"))
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)
	return pgmigrate.CheckVersions(ctx, conn, migrations, versionTable)
}

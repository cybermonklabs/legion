package store

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/pgdsn"
)

// censusApplicationName is how the census's one connection appears in pg_stat_activity.
const censusApplicationName = "envoy-dispatch census"

// Census takes the pre-deploy census (pgmigrate.Census) of the database databaseURL names against
// the migrations this binary embeds, on one connection of its own: a deployment runs it before it
// rolls the service, so it uses no pool and migrates nothing. It refuses and names a DSN as Open
// does (pgdsn).
func Census(ctx context.Context, databaseURL string) (*pgmigrate.Report, error) {
	return census(ctx, databaseURL, migrationFiles, pgmigrate.CensusOptions{})
}

func census(ctx context.Context, databaseURL string, fsys fs.FS, options pgmigrate.CensusOptions) (*pgmigrate.Report, error) {
	migrations, err := pgmigrate.Load(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrations refused: %w", err)
	}
	return censusOf(ctx, databaseURL, migrations, options)
}

// censusOf is census of a set already loaded: a test that takes the census of many prefixes of
// one set slices the set it loaded once rather than loading each prefix again.
func censusOf(ctx context.Context, databaseURL string, migrations []pgmigrate.Migration, options pgmigrate.CensusOptions) (*pgmigrate.Report, error) {
	if err := pgdsn.Refusal(DSNSetting, databaseURL); err != nil {
		return nil, err
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, pgdsn.ConnectError(DSNSetting, databaseURL, err)
	}
	config.RuntimeParams["application_name"] = censusApplicationName
	// The census's one author-written statement is pinned to the extended protocol on its own
	// call (pgmigrate.Census); this pins the connection's default too, so a connection string
	// asking for the simple protocol changes nothing the census runs.
	config.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, pgdsn.ConnectError(DSNSetting, databaseURL, err)
	}
	defer conn.Close(ctx)
	return pgmigrate.Census(ctx, conn, migrations, "schema_migrations", options)
}

// CheckVersions reads what schema_migrations, on the database databaseURL names, records of each
// version against the migrations this binary embeds, and what the runner's next boot would do
// with each row (pgmigrate.CheckVersions). It holds one connection of its own, in a read-only
// transaction, and writes nothing. It refuses and names a DSN as Open does (pgdsn).
func CheckVersions(ctx context.Context, databaseURL string) (*pgmigrate.VersionCheck, error) {
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrations refused: %w", err)
	}
	if err := pgdsn.Refusal(DSNSetting, databaseURL); err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, pgdsn.ConnectError(DSNSetting, databaseURL, err)
	}
	defer conn.Close(ctx)
	return pgmigrate.CheckVersions(ctx, conn, migrations, versionTable)
}

// UnappliedMigrations answers the version of every migration this binary embeds that
// schema_migrations, read in tx, records no row of, in the order the runner applies them: every
// one of them where the table does not exist yet (pgmigrate.RecordedVersions). It writes nothing,
// so a reader that needs a database migrated to this build - the store census - asks it in its own
// read-only snapshot rather than probing a table or a column of one migration.
func UnappliedMigrations(ctx context.Context, tx pgx.Tx) ([]int, error) {
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrations refused: %w", err)
	}
	recorded, err := pgmigrate.RecordedVersions(ctx, tx, versionTable)
	if err != nil {
		return nil, err
	}
	var unapplied []int
	for _, migration := range migrations {
		if !recorded[migration.Version] {
			unapplied = append(unapplied, migration.Version)
		}
	}
	return unapplied, nil
}

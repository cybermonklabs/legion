package store

import (
	"context"
	"io/fs"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The helpers this package's own tests migrate a database with, for its external tests
// (package store_test), which import packages that import this one - graphcensus, whose census
// reads what a migration wrote - and so cannot be tests of this package.
var (
	OpenEmptyTestStore = openEmptyTestStore
	MigrateThrough     = migrateThrough
)

// CensusOf is the pre-deploy census over the migrations fsys holds, on the census's own
// connection, for TestEveryConnectionOfThePoolCarriesTheStoresMovedWriterMark, which imports
// processes and so is an external test: a census file of its own reads what that connection
// carries.
func CensusOf(ctx context.Context, databaseURL string, fsys fs.FS) (*pgmigrate.Report, error) {
	return census(ctx, databaseURL, fsys, pgmigrate.CensusOptions{})
}

// HealthPool is the health probe's pool (healthPool), which no caller is handed, for the same test.
func (p *Pool) HealthPool() (*pgxpool.Pool, error) { return p.healthPool() }

// CensusOfEveryMigration is the pre-deploy census over every migration this binary embeds, under
// options, for TestStoresMovedPassesTheCensusAtAOneByteLimit, which reads the stores' move's list of
// this issue's own migrations (storesmove.OwnMigrations) and so is an external test.
func CensusOfEveryMigration(ctx context.Context, databaseURL string, options pgmigrate.CensusOptions) (*pgmigrate.Report, error) {
	return census(ctx, databaseURL, migrationFiles, options)
}

// MigrateWithout is Migrate over every embedded migration but those named, which it leaves
// pending, through the runner, which records each version it applies.
func (s *Store) MigrateWithout(ctx context.Context, names []string) error {
	return s.migrate(ctx, migrationsLeftOut{FS: migrationFiles, names: names})
}

// migrationsLeftOut is the embedded set less the files of names and theirs (a migration's census
// and rollback script share its version and name).
type migrationsLeftOut struct {
	fs.FS
	names []string
}

func (m migrationsLeftOut) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(m.FS, name)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(entries, func(entry fs.DirEntry) bool {
		return slices.ContainsFunc(m.names, func(left string) bool {
			stem := left[:len(left)-len(".up.sql")]
			return entry.Name() == left || entry.Name() == stem+".census.sql" || entry.Name() == stem+".down.sql"
		})
	}), nil
}

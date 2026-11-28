package store

// The helpers this package's own tests migrate a database with, for its external tests
// (package store_test), which import packages that import this one - graphcensus, whose census
// reads what a migration wrote - and so cannot be tests of this package.
var (
	OpenEmptyTestStore = openEmptyTestStore
	MigrateThrough     = migrateThrough
)

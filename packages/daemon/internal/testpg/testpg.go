// Package testpg gives a test a Postgres database carrying the daemon's schema on the devbox and
// CI Postgres that LEGION_TEST_PG_DSN names: Clone, ClonePool and CloneStore copy a template this
// process migrated once (template.go), each clone dropped when the test ends, and the package
// whose tests call any of them returns Main from its TestMain, which drops the template. A test is
// skipped without that server: there is nothing to run against.
//
// Migrating the template needs internal/store, so this package imports it, and a test binary of
// internal/store cannot import this one back. The half of the fixture that needs no schema is
// internal/testpg/testdb, which imports no package of this module except internal/testwait, which
// imports none, and so is open to every package, internal/store among them: an empty database
// (testdb.Database), a file holding its DSN (testdb.DSNFile), a database reached across a network's
// delay (testdb.Delayed) or through a proxy that holds or cuts a commit's reply (testdb.CommitFaults),
// a pool whose transactions are cancelled between two statements (testdb.CancelAfterScan), and the
// drop that waits out an autovacuum worker and a backend that has not exited yet (testdb.Drop),
// which every clone of this package's template and the template itself go through. The size of
// every pool a test builds is Envoy's public testpool (testpool.PoolConns, testpool.NewPool), which
// ClonePool's pool takes and both Go modules import.
package testpg

// Package testpg gives a test a Postgres database carrying the daemon's schema on the devbox and
// CI Postgres that LEGION_TEST_PG_DSN names: Clone, ClonePool and CloneStore copy a template this
// process migrated once (template.go), each clone dropped when the test ends, and the package
// whose tests call any of them returns Main from its TestMain, which drops the template. A test is
// skipped without that server: there is nothing to run against.
//
// Migrating the template needs internal/store, so this package imports it, and a test binary of
// internal/store cannot import this one back. The half of the fixture that needs no schema is
// internal/testpg/testdb, which imports no package of this module except internal/testwait, which
// imports none, and so is open to every package, internal/store among them. Its package comment
// says what it holds; every clone of this package's template, and the template itself, is dropped
// through its Drop.
package testpg

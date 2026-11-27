// Package testcleanup registers a test's cleanup so that the test stops holding what the cleanup
// captured once the cleanup has run.
//
// A test's own t.Cleanup does not: testing runs a cleanup by shortening the test's slice of them
// (common.runCleanup, `c.cleanups = c.cleanups[:last]`), which leaves the closure in the slice's
// backing array, and it keeps a parallel test's *T in its parent's list of parallel subtests until
// every one of them has ended. For a top-level parallel test that parent is the test binary's root,
// so a finished test's cleanups, and the service, document or connection each one captured, stay
// live until the binary's last parallel test ends. In internal/dispatch/docs, where most tests build
// a document service and many a 400 KB document's room, its peers and a shadow of it, that kept
// the live heap of every finished test: the race binary grew through the run to 8.6 GB, past the
// 8 GB envoy-go-race-docs allows it, at its end and not at any one test.
//
// Add wraps the cleanup in one that drops its reference to it once it has run, so the closure
// testing keeps holds nothing. It is an ordinary package rather than a _test.go file because Go
// cannot share test-only code across packages.
package testcleanup

import "testing"

// Add registers f to run when tb and its subtests end, as tb.Cleanup(f) does, and drops tb's
// reference to f once it has run.
func Add(tb testing.TB, f func()) {
	tb.Cleanup(func() {
		run := f
		f = nil
		run()
	})
}

// Package samename has two parallel tests of one name, TestSame: this one of package samename
// passes, and the one of package samename_test fails. Both runs start before either ends, and the
// report names each event by the test's name alone, so go test's converter gives both end events
// to the second run.
package samename

import "testing"

func TestSame(t *testing.T) { t.Parallel() }

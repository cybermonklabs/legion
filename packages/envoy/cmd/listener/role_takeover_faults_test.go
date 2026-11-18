package main

import "testing"

// setTakeoverFaults puts faults in force for the rest of t (takeoverFaults), and none once t ends.
// The tests that force a fault of the takeover copy do not run in parallel with one another, since
// the seam set is the process's.
func setTakeoverFaults(t testing.TB, faults takeoverFaultSet) {
	t.Helper()
	takeoverFaults.Store(&faults)
	t.Cleanup(func() { takeoverFaults.Store(nil) })
}

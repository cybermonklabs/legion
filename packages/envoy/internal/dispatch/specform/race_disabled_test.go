//go:build !race

package specform

// raceEnabled reports whether the package was built with the race detector. The work table
// (work_test.go) reads it: under the detector, whose instrumentation makes each judgment about 15
// times the work, it runs each row at a smaller n (workRaceShrink), to the same bound, since the
// plain build holds the bound at the n each planted walk was measured at, and a detector finds no
// shared state in a judgment of one goroutine.
const raceEnabled = false

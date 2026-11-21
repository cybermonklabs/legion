//go:build !race

package specform

// raceEnabled reports whether the package was built with the race detector. The work table
// (work_test.go) reads it: under the detector, whose instrumentation makes each judgment about 15
// times the work, it runs each row at a smaller n and claims no bound, since the plain build holds
// the bound and a detector finds no shared state in a measure of one thread.
const raceEnabled = false

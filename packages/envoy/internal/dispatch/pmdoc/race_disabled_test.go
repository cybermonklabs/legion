//go:build !race

package pmdoc

// raceEnabled reports whether the package was built with the race detector. What reads it is the
// CPU-time checks of the heavy-bodies rows (cpuGrowsLinearly, heavy_bodies_test.go), which run only
// without it: the detector multiplies the CPU time every call spends, some five times, and by an
// amount its instrumentation decides, so a CPU time measured under it says nothing of the parse,
// and those checks check no concurrency the detector could catch.
const raceEnabled = false

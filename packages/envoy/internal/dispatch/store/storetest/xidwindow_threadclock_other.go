//go:build !linux

package storetest

// xidThreadClocksAvailable is false: this build names no thread's scheduler clocks, so the rig
// cannot tell the code's work between two statements from the time the machine kept it waiting
// for a CPU, and judges no window's gaps (XidJudgesGaps).
func xidThreadClocksAvailable() bool { return false }

// xidThread answers 0: this build names no thread.
func xidThread() int { return 0 }

// readXidThreadClock answers the zero clock: this build reads no thread's scheduler clocks.
func readXidThreadClock() xidThreadClock { return xidThreadClock{} }

// xidHoldThread holds nothing: a build that judges no gap has no thread to keep still.
func xidHoldThread() (release func(), held bool) { return func() {}, true }

//go:build !linux

package storetest

// xidThread answers 0: this build names no thread.
func xidThread() int { return 0 }

// readXidThreadClock answers the zero clock: this build reads no thread's scheduler clocks, so the
// probe finds gaps cannot be judged (xidThreadClocksUnavailable).
func readXidThreadClock() xidThreadClock { return xidThreadClock{} }

// xidHoldThread holds nothing: a build that judges no gap has no thread to keep still.
func xidHoldThread() (release func(), held bool) { return func() {}, true }

// xidWatchThreadClock opens nothing: this build reads no thread's scheduler clocks.
func xidWatchThreadClock() (stop func(), ok bool) { return func() {}, false }

// xidDropThreadClock drops nothing: this build holds no thread's clock.
func xidDropThreadClock(int) {}

// xidHoldMemory holds nothing: a build that judges no gap has no window's memory to keep still.
func xidHoldMemory() bool { return true }

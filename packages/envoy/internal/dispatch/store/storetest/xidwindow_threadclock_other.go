//go:build !linux

package storetest

import (
	"errors"
	"time"
)

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

// xidHoldMemory holds nothing: a build that judges no gap has no window's memory to keep still.
func xidHoldMemory() bool { return true }

// xidTrace and xidTraceReading are the execution trace a build that judges gaps takes of the
// operation; this build judges none and takes none (XidJudgesGaps).
type xidTrace struct{}

type xidTraceReading struct{}

func startXidTrace() (*xidTrace, error) {
	return nil, errors.New("this build judges no gap, so it takes no execution trace")
}

func (*xidTrace) stop() {}

func (*xidTrace) read() (*xidTraceReading, error) { return &xidTraceReading{}, nil }

func (*xidTraceReading) goStates(int, time.Time, time.Time) (runnable, waiting time.Duration, ok bool) {
	return 0, 0, false
}

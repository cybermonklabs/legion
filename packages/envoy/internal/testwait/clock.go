package testwait

import "time"

// clock is the time the waits here read and arm their timer by: the wall clock. This package's own
// tests give a child test binary a clock the child moves itself (stepClock, in testwait_test.go), so
// a wait's margin passes at a step the child takes, never at a moment a loaded machine moves, and
// the child's verdict follows from what its waits did, not from how long the machine took to do it.
var clock interface {
	// Now is the time now.
	Now() time.Time
	// Timer returns a channel that receives once at has come, and the function that releases it.
	Timer(at time.Time) (<-chan time.Time, func() bool)
} = wallClock{}

// wallClock is the time as the machine keeps it.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) Timer(at time.Time) (<-chan time.Time, func() bool) {
	timer := time.NewTimer(time.Until(at))
	return timer.C, timer.Stop
}

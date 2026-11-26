package testwait

import (
	"testing"
	"testing/synctest"
	"time"
)

// This file holds the shared wait's forms for a testing/synctest bubble: Idle, the proof of an
// absence there (the package doc's "Proving an absence") for code that a timer of its own may let
// act late, and Bubble, which runs a bubble whose t a wait of this package takes.

// Idle proves an absence in a synctest bubble past a timer of the code's own: d is the caller's
// span, which outlasts that timer, and why names the timer and says why d outlasts it; an empty
// why fails the test, as Span's and Quiet's does. This package holds no span of its own. Idle runs
// on a goroutine of a bubble: outside one, synctest.Wait panics.
//
// It waits for three things in turn. First, every other goroutine of the bubble is durably blocked
// (synctest.Wait). Then d passes on the bubble's clock, which moves only while every goroutine of
// the bubble is durably blocked, and then only to the next moment a timer is due: the timers due
// within d fire in their order, timers armed during d among them, and what each wakes runs until
// it blocks again before the clock moves on. Last, every other goroutine is durably blocked once
// more (synctest.Sleep), so what a timer due at the very end of d woke has run too. Idle returns
// then, with the bubble's clock d past where it found it.
//
// An absence read after Idle proves that the bubble's goroutines, left to themselves for d on the
// bubble's clock with every timer due within it fired, did not make the thing happen. It proves
// nothing past d: a timer of the code's own due later lets the code act after the read, which is
// why the caller names the timer d outlasts. It reaches no goroutine outside the bubble, which
// synctest does not wait for. A goroutine of the bubble blocked where it is not durably blocked (a
// network socket, a system call, a sync.Mutex) holds Idle until it blocks durably or ends.
func Idle(t testing.TB, d time.Duration, why string) {
	t.Helper()
	requireWhy(t, "Idle", why, "which timer of the code's own its span outlasts, and why")
	synctest.Wait()
	synctest.Sleep(d)
}

// Bubble runs f in a testing/synctest bubble (synctest.Test) started on t, and hands f the
// bubble's own t with a Deadline that a wait of this package takes inside the bubble, where the
// bubble's t panics on Deadline. Before the bubble starts, Bubble reads on the real clock how much
// of t's deadline is left; the t f takes names the moment that much past the bubble's start, on
// the bubble's clock, which starts at midnight UTC 2000-01-01. So a wait in the bubble gives up the
// margin before that moment and fails naming what it waited for: a wait that asks at the
// package's pace (Until, Eventually) moves the bubble's clock by the pace between two asks while
// nothing else in the bubble runs, and a Receive whose value never comes gives up once nothing
// else in the bubble can run, since the bubble's clock then moves to its timer.
//
// The bubble's clock moves only while every goroutine of the bubble is durably blocked, and then
// at once to the next timer due. In a bubble whose goroutines spend their time durably blocked,
// its waits among them, that clock runs ahead of the real one, and a wait there gives up before
// the test binary's deadline comes. One whose goroutines hold its clock still for long, by running
// or by blocking where they are not durably blocked (a network socket, a system call, a
// sync.Mutex), reaches its give-up later on the real clock, and go test's alarm can come first.
// Under -timeout 0 t names no deadline, neither does the t f takes, and no wait in the bubble has
// a timer.
func Bubble(t *testing.T, f func(t testing.TB)) {
	t.Helper()
	deadline, bounded := t.Deadline()
	left := time.Until(deadline)
	synctest.Test(t, func(t *testing.T) {
		f(bubbled{T: t, deadline: time.Now().Add(left), bounded: bounded})
	})
}

// bubbled is the t Bubble hands its function: the bubble's own t, with the Deadline Bubble named
// on the bubble's clock.
type bubbled struct {
	*testing.T
	deadline time.Time
	bounded  bool
}

func (b bubbled) Deadline() (time.Time, bool) { return b.deadline, b.bounded }

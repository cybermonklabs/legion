// Package fataltest runs a test helper that may fail its test with Fatal or Fatalf, and answers
// what the helper failed with rather than failing the test: so a test can require that a helper
// fails, and what its failure says, where the helper's t.Fatal would end the test itself. The
// helper runs on a goroutine of its own over a Recorder of the test, which ends that goroutine at
// the failure as t.Fatal does (runtime.Goexit), so the helper stops where it failed. Failure waits
// for the goroutine; Start leaves the wait to its caller, for a test that bounds it itself.
package fataltest

import (
	"fmt"
	"runtime"
	"testing"
)

// Recorder is t with Fatal and Fatalf recorded and their goroutine ended, as t.Fatal does; every
// other method is t's own.
type Recorder struct {
	testing.TB
	message string
}

// Fatal records args, as t.Fatal would print them, and ends the calling goroutine.
func (r *Recorder) Fatal(args ...any) {
	r.message = fmt.Sprint(args...)
	runtime.Goexit()
}

// Fatalf records the formatted failure, as t.Fatalf would print it, and ends the calling goroutine.
func (r *Recorder) Fatalf(format string, args ...any) {
	r.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// Message is what Fatal or Fatalf recorded, "" when neither ran. Read it once the goroutine that
// fails through r has ended (Start's done).
func (r *Recorder) Message() string { return r.message }

// Start runs call over a Recorder of t on a goroutine of its own, and answers the recorder and a
// channel closed once call returned or failed.
func Start(t testing.TB, call func(testing.TB)) (*Recorder, <-chan struct{}) {
	recorder := &Recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		call(recorder)
	}()
	return recorder, done
}

// Failure runs call as Start does and waits for it to end: what it failed with, or "" when it
// returned.
func Failure(t testing.TB, call func(testing.TB)) string {
	recorder, done := Start(t, call)
	<-done
	return recorder.Message()
}

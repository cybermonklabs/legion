package fataltest

import (
	"testing"
	"time"
)

// Failure answers what a helper failed with, by Fatal or Fatalf, and the helper stops there, as it
// would under t.Fatal; a helper that returns answers "". The test itself goes on either way.
func TestFailureAnswersTheFailureAndStopsTheHelperThere(t *testing.T) {
	ranOn := false
	if got := Failure(t, func(t testing.TB) { t.Fatal("no lock for ", 7); ranOn = true }); got != "no lock for 7" {
		t.Fatalf("Failure of a Fatal answered %q, want %q", got, "no lock for 7")
	}
	if got := Failure(t, func(t testing.TB) { t.Fatalf("waited %s", time.Second); ranOn = true }); got != "waited 1s" {
		t.Fatalf("Failure of a Fatalf answered %q, want %q", got, "waited 1s")
	}
	if ranOn {
		t.Fatal("a helper ran on past its Fatal")
	}
	if got := Failure(t, func(testing.TB) {}); got != "" {
		t.Fatalf("Failure of a helper that returned answered %q, want none", got)
	}
}

// Start leaves the wait to its caller: done closes once the helper failed, and Message then reads
// what it failed with.
func TestStartClosesDoneOnceTheHelperFailed(t *testing.T) {
	release := make(chan struct{})
	recorder, done := Start(t, func(t testing.TB) {
		<-release
		t.Fatal("released")
	})
	select {
	case <-done:
		t.Fatal("done closed before the helper failed")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("done did not close within 10s of the helper's failure")
	}
	if got := recorder.Message(); got != "released" {
		t.Fatalf("Message answered %q, want %q", got, "released")
	}
}

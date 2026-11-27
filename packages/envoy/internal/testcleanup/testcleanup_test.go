package testcleanup

import (
	"runtime"
	"testing"
	"weak"
)

// What a cleanup captured stays live while testing holds the finished test when the cleanup was
// registered with t.Cleanup, and is released when it was registered with Add. The subtest's *T is
// kept here the way testing keeps a finished parallel test in its parent's list of parallel
// subtests until they have all ended.
func TestAFinishedTestKeepsNothingItsCleanupCaptured(t *testing.T) {
	for _, test := range []struct {
		name     string
		register func(testing.TB, func())
		kept     bool
	}{
		// The control: if testing ever drops a cleanup it has run, this row fails and this
		// package is no longer needed.
		{"t.Cleanup", func(tb testing.TB, f func()) { tb.Cleanup(f) }, true},
		{"Add", Add, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var held *testing.T
			var captured weak.Pointer[[1 << 20]byte]
			ran := false
			t.Run("finished", func(t *testing.T) {
				held = t
				block := new([1 << 20]byte)
				captured = weak.Make(block)
				test.register(t, func() { block[0] = 1; ran = true })
			})
			if !ran {
				t.Fatal("the cleanup did not run when its test ended")
			}
			runtime.GC()
			if kept := captured.Value() != nil; kept != test.kept {
				t.Fatalf("what the finished test's cleanup captured is kept = %t, want %t", kept, test.kept)
			}
			runtime.KeepAlive(held)
		})
	}
}

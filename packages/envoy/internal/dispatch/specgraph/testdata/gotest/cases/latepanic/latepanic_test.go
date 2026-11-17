// Package latepanic has a test that passes and leaves a goroutine behind, which panics after
// TestMain's m.Run returns, so the package fails after the test's own end.
package latepanic

import (
	"os"
	"testing"
	"time"
)

var done = make(chan struct{})

func TestMain(m *testing.M) {
	code := m.Run()
	close(done)
	time.Sleep(time.Second)
	os.Exit(code)
}

func TestCompleted(t *testing.T) {
	go func() {
		<-done
		panic("work that the test started panics after its end")
	}()
}

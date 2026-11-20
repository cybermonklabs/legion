// Package killfirst has a test that sleeps past -timeout 1s and two tests after it, which never
// start, because go test kills the test binary first.
package killfirst

import (
	"testing"
	"time"
)

func TestSleepsPastTheTimeout(t *testing.T) {
	t.Log("sleeps for 5 s under -timeout 1s")
	time.Sleep(5 * time.Second)
}

func TestNeverStartsOne(t *testing.T) {}

func TestNeverStartsTwo(t *testing.T) {}

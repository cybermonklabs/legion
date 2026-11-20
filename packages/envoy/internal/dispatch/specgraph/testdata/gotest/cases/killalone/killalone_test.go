// Package killalone is the one test of its package, and it sleeps past -timeout 1s, so go test
// kills it.
package killalone

import (
	"testing"
	"time"
)

func TestSleepsPastTheTimeout(t *testing.T) {
	t.Log("sleeps for 5 s under -timeout 1s")
	time.Sleep(5 * time.Second)
}

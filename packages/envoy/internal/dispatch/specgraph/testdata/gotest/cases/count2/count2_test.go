// Package count2 runs under -count=2: each test runs two times, and each run of a test starts with
// its own run event.
package count2

import "testing"

func TestSequentialPasses(t *testing.T) {}

func TestParallelPasses(t *testing.T) { t.Parallel() }

func TestParallelFails(t *testing.T) {
	t.Parallel()
	t.Fail()
}

package eleven

import (
	"testing"

	"example.com/gotestfixtures/state"
)

// Eleven tests that each fail on their first attempt and pass on any later one: one more than
// gotestsum reruns by default (--rerun-fails-max-failures, 10).
func failsOnce(t *testing.T) {
	t.Helper()
	if state.Attempt(t, t.Name()) == 1 {
		t.Fatal("fails on its first attempt")
	}
}

func TestFlake01(t *testing.T) { failsOnce(t) }
func TestFlake02(t *testing.T) { failsOnce(t) }
func TestFlake03(t *testing.T) { failsOnce(t) }
func TestFlake04(t *testing.T) { failsOnce(t) }
func TestFlake05(t *testing.T) { failsOnce(t) }
func TestFlake06(t *testing.T) { failsOnce(t) }
func TestFlake07(t *testing.T) { failsOnce(t) }
func TestFlake08(t *testing.T) { failsOnce(t) }
func TestFlake09(t *testing.T) { failsOnce(t) }
func TestFlake10(t *testing.T) { failsOnce(t) }
func TestFlake11(t *testing.T) { failsOnce(t) }

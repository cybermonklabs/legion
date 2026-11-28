package daemon

import (
	"testing"
	"time"
)

// CONTROL (c-daemon-ci-budget): a test that holds the leg its group runs in for 900 seconds, so
// the three legs' package lines add to more than 1,500 seconds while no leg reaches its own
// timeout. It is no product test and runs on no branch but its control's.
func TestTheBudgetControlHoldsOneLeg(t *testing.T) {
	time.Sleep(900 * time.Second)
}

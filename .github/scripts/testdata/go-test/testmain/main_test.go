package testmain

import (
	"fmt"
	"os"
	"testing"
)

// leftBehind is the state TestLeavesState leaves and nothing clears.
var leftBehind bool

// TestMain fails the package after its tests pass whenever they left state behind: a check that
// needs the tests to have run, so a rerun that runs no test (gotestsum's -test.run=^$ for a
// package that failed with no failed test) passes it.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 && leftBehind {
		fmt.Println("TestMain: a test left state behind")
		code = 1
	}
	os.Exit(code)
}

func TestLeavesState(t *testing.T) { leftBehind = true }

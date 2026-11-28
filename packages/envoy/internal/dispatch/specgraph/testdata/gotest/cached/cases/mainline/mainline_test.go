package mainline

import (
	"fmt"
	"os"
	"testing"
)

// TestMain prints, after the tests, a run line of TestZ with the testing package's marker, so go
// test's converter names TestZ in each event after it, go test's summary line of the package
// among them.
func TestMain(m *testing.M) {
	code := m.Run()
	fmt.Print("\x16=== RUN   TestZ\n")
	os.Exit(code)
}

func TestA(t *testing.T) {}

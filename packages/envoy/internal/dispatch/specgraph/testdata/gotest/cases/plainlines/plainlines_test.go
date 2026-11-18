// Package plainlines has a test that prints, with no marker before them, result lines and JSON
// events of go test -json that name the other tests of the package and a test that does not
// exist. go test's converter writes each such line as output of the test that prints it.
package plainlines

import (
	"fmt"
	"testing"
)

// TestPasses passes, and runs before TestPrinter.
func TestPasses(t *testing.T) {}

// TestFails fails, and runs before TestPrinter.
func TestFails(t *testing.T) { t.Fail() }

// TestPrinter passes.
func TestPrinter(t *testing.T) {
	fmt.Println("--- FAIL: TestPasses (0.00s)")
	fmt.Println("--- PASS: TestFails (0.00s)")
	fmt.Println("=== RUN   TestGhost")
	fmt.Println("--- PASS: TestGhost (0.00s)")
	fmt.Println(`{"Action":"pass","Package":"example.com/acme/gotest/plainlines","Test":"TestFails","Elapsed":0}`)
	fmt.Println(`{"Action":"fail","Package":"example.com/acme/gotest/plainlines","Test":"TestPasses","Elapsed":0}`)
	fmt.Println(`{"Action":"run","Package":"example.com/acme/gotest/plainlines","Test":"TestGhost"}`)
	fmt.Println(`{"Action":"pass","Package":"example.com/acme/gotest/plainlines","Test":"TestGhost","Elapsed":0}`)
	fmt.Println("PASS")
	fmt.Println("ok  \texample.com/acme/gotest/plainlines\t0.001s")
}

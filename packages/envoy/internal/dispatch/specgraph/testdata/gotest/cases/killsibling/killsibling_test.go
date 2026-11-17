// Package killsibling has two parallel tests. TestSleeps logs a line after TestPrinter runs, so go
// test's converter names TestSleeps last. Then TestPrinter prints, with no marker, go test's fail
// line of TestSleeps, which the converter gives to TestSleeps, and passes. TestSleeps sleeps past
// -timeout 1s, so go test kills it.
package killsibling

import (
	"fmt"
	"testing"
	"time"
)

var (
	printerRuns  = make(chan struct{})
	sleepsLogged = make(chan struct{})
	printed      = make(chan struct{})
)

func TestSleeps(t *testing.T) {
	t.Parallel()
	<-printerRuns
	t.Log("TestSleeps starts")
	close(sleepsLogged)
	<-printed
	t.Log("TestSleeps sleeps")
	time.Sleep(5 * time.Second)
}

func TestPrinter(t *testing.T) {
	t.Parallel()
	close(printerRuns)
	<-sleepsLogged
	fmt.Println("--- FAIL: TestSleeps (0.00s)")
	close(printed)
}

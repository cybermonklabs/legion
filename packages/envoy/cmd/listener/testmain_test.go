package main

import (
	"flag"
	"os"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	dispatchstoretest "github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// testMainHarness runs the package's tests inside whatever else their build needs around
// testnats.Main: nothing, unless a tagged file sets it. Under natslive, whose relay socket rows
// start a broker, testmain_natslive_test.go sets it to storetest.Run, so only that build links the
// broker's test database harness. The hook stands in for a !natslive file, which
// go-tagged-tests.sh refuses: a tag is read only in its bare form.
var testMainHarness = func(run func() int) int { return run() }

// TestMain sets the package's default -parallel (listenerTestParallelism), and removes the NATS
// server the package's tests share (testnats.Main, started with listenerTestAccounts accounts),
// inside testMainHarness, inside Dispatch's storetest.Run, which drops the databases the
// dispatch_resolution rows open (migratedDispatchDatabase), inside cmdtest.Run, which removes the
// binaries they built in every build.
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(listenerTestParallelism)); err != nil {
		panic("set this package's default -parallel: " + err.Error())
	}
	os.Exit(cmdtest.Run(func() int {
		return dispatchstoretest.Run(func() int {
			return testMainHarness(func() int { return testnats.Main(m, testnats.Accounts(listenerTestAccounts)) })
		})
	}))
}

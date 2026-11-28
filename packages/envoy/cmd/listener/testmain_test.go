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

// TestMain sets the package's default -parallel (listenerTestParallelism), and removes the NATS
// server the package's tests share (testnats.Main, started with listenerTestAccounts accounts),
// inside Dispatch's storetest.Run, which drops the databases the dispatch_resolution rows open
// (migratedDispatchDatabase), inside cmdtest.Run, which removes the binaries they built in every
// build.
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(listenerTestParallelism)); err != nil {
		panic("set this package's default -parallel: " + err.Error())
	}
	os.Exit(cmdtest.Run(func() int {
		return dispatchstoretest.Run(func() int { return testnats.Main(m, testnats.Accounts(listenerTestAccounts)) })
	}))
}

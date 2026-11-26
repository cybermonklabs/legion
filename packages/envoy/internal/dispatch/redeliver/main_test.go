package redeliver_test

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain runs the package's tests storetest.DefaultParallel at a time (SetDefaultParallel).
//
// It gives the package both harnesses: testnats.Main runs the tests and shuts down the one
// NATS server they share, each test in a JetStream account of its own (openState), and
// storetest.Run then drops the databases they cloned.
func TestMain(m *testing.M) {
	storetest.SetDefaultParallel()
	os.Exit(storetest.Run(func() int { return testnats.Main(m) }))
}

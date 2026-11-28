package main

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// testMainHarness runs the package's tests inside whatever else their build needs around
// testnats.Main: nothing, unless a tagged file sets it. Under natslive, whose relay socket rows
// start a broker, testmain_natslive_test.go sets it to storetest.Run, so only that build links the
// broker's test database harness. The hook stands in for a !natslive file, which
// go-tagged-tests.sh refuses: a tag is read only in its bare form.
var testMainHarness = func(run func() int) int { return run() }

// TestMain removes the NATS server the package's tests share (testnats.Main), inside
// testMainHarness, inside cmdtest.Run, which removes the binaries they built in every build.
func TestMain(m *testing.M) {
	os.Exit(cmdtest.Run(func() int { return testMainHarness(func() int { return testnats.Main(m) }) }))
}

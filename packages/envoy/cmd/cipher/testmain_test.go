package main

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// TestMain runs the package's tests through storetest.Main, which drops the test binary's own
// database that storetest.URL makes each test's schema in, inside cmdtest.Run, which removes the
// binaries they built. A command a test runs in this process (runIn) starts no sign-in renewer:
// os.Executable is the test binary here, not agent-secrets. The renewer's own tests run the built
// binary, which starts one (signin_renewer_linux_test.go).
func TestMain(m *testing.M) {
	launchRenewer = func(keptSignIn) error { return nil }
	os.Exit(cmdtest.Run(func() int { return storetest.Main(m) }))
}

package main

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// TestMain removes the binaries the package's tests built (cmdtest.Main). A command a test runs in
// this process (runIn) starts no sign-in renewer: os.Executable is the test binary here, not
// agent-secrets. The renewer's own tests run the built binary, which starts one
// (signin_renewer_linux_test.go).
func TestMain(m *testing.M) {
	launchRenewer = func(keptSignIn) error { return nil }
	os.Exit(cmdtest.Main(m))
}

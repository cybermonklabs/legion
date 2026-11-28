package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// TestMain removes the binaries the package's tests built (cmdtest.Main), and then the directory
// of the overlay the exec rows build agent-secrets with (removeBackoffOverlay). A command a test
// runs in this process (runIn) starts no sign-in renewer: os.Executable is the test binary here,
// not agent-secrets. The renewer's own tests run the built binary, which starts one
// (signin_renewer_linux_test.go).
func TestMain(m *testing.M) {
	launchRenewer = func(keptSignIn) error { return nil }
	code := cmdtest.Main(m)
	if err := removeBackoffOverlay(); err != nil {
		fmt.Fprintf(os.Stderr, "remove the backoff hook's overlay: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

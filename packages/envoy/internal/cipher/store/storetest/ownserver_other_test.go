//go:build !linux

package storetest

import "os/exec"

// startServer starts server, the test's own postgres (ownServer). A parent-death signal is Linux's
// alone, so on this system a test binary that ends without running the cleanup that stops postgres
// leaves it running; ownServer's comment says how to find it and stop it.
func startServer(server *exec.Cmd) error {
	return server.Start()
}

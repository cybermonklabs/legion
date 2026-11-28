package storetest

import (
	"os/exec"
	"runtime"
	"syscall"
)

// startServer starts server, the test's own postgres (ownServer), with SIGINT, its fast shutdown,
// as its parent-death signal: where the test binary ends without running the cleanup that stops
// postgres, killed or ended by go test's alarm (-timeout), the kernel sends postgres that SIGINT,
// so it does not run on with nothing left to stop it. The kernel sends it when the thread that
// started postgres ends, not the process (go.dev/issue/27505), and the Go runtime ends a thread
// only when the goroutine locked to it returns still locked. So Start runs on a thread this
// goroutine holds, which it lets go of once Start returns rather than holding it to the
// goroutine's return, which would end the thread and postgres with it: let go, the thread goes
// back to the runtime, which keeps it until the process ends.
func startServer(server *exec.Cmd) error {
	server.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGINT}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return server.Start()
}

package sockettest

import (
	"net"
	"os"
	"runtime"
	"slices"
	"syscall"
	"testing"
)

// A listening socket this process holds on two descriptors, the listener's and a copy of it, is
// found on exactly those two, in ascending order; once both are closed it is found on none.
func TestDescriptorsAreEveryCopyOfTheSocketThisProcessHolds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("Descriptors reads Linux's /proc, and this is %s", runtime.GOOS)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on a loopback port: %v", err)
	}
	defer listener.Close()
	copied, err := listener.(*net.TCPListener).File()
	if err != nil {
		t.Fatalf("copy the listener's descriptor: %v", err)
	}
	defer copied.Close()
	raw, err := listener.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatalf("read the listener's descriptor: %v", err)
	}
	var own int
	if err := raw.Control(func(fd uintptr) { own = int(fd) }); err != nil {
		t.Fatalf("read the listener's descriptor: %v", err)
	}
	info, err := copied.Stat()
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	inode := info.Sys().(*syscall.Stat_t).Ino

	want := []int{own, int(copied.Fd())}
	slices.Sort(want)
	if got, err := Descriptors(os.Getpid(), inode); err != nil || !slices.Equal(got, want) {
		t.Fatalf("Descriptors(this process, the socket) = %v, %v; want %v, nil", got, err, want)
	}
	_ = copied.Close()
	_ = listener.Close()
	if got, err := Descriptors(os.Getpid(), inode); err != nil || len(got) != 0 {
		t.Fatalf("Descriptors(this process, the closed socket) = %v, %v; want none, nil", got, err)
	}
}

// A process with no /proc entry, as one that has exited and been reaped, holds no descriptor.
// Linux's pid_max is at most 4194304 (1<<22), so no process has the pid above it.
func TestAProcessThatIsGoneHoldsNone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("Descriptors reads Linux's /proc, and this is %s", runtime.GOOS)
	}
	if got, err := Descriptors(1<<22+1, 1); err != nil || len(got) != 0 {
		t.Fatalf("Descriptors(a pid no process has) = %v, %v; want none, nil", got, err)
	}
}

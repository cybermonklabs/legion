package procgroup

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

// ControlHolds counts the runs ControlHold is asked about. CONTROL ONLY (r63 hold), never merged.
var ControlHolds atomic.Int64

// ControlHold gives the first run of each process, and every second one after it, at *out, a pipe
// already full that nothing reads until done has ended and 50 ms more, so the command's first
// write there waits for its budget's stop. CONTROL ONLY (r63 hold), never merged. It answers a func
// for the caller to defer, which closes the pipe's write end once the command is gone.
func ControlHold(out *io.Writer, done context.Context) func() {
	if ControlHolds.Add(1)%2 != 1 {
		return func() {}
	}
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	size, _, errno := syscall.Syscall(syscall.SYS_FCNTL, w.Fd(), 1032, 0) // F_GETPIPE_SZ
	if errno != 0 {
		panic(errno)
	}
	if _, err := w.Write(make([]byte, size)); err != nil {
		panic(err)
	}
	*out = w
	go func() {
		<-done.Done()
		time.Sleep(50 * time.Millisecond)
		_, _ = io.Copy(io.Discard, r)
		_ = r.Close()
	}()
	return func() { _ = w.Close() }
}

package capabilities

import (
	"context"
	"io"
	"os"
	"syscall"
	"time"
)

// controlHold3 gives every command runWithin starts, at *stdout and *stderr, pipes already full that
// nothing reads until 3 s have passed or the attempt has ended, whichever is first, and 50 ms more:
// a check that holds its command's output, so the command's first write waits up to 3 s. What the
// command wrote past the filler still reaches the writers *stdout and *stderr held before. CONTROL
// ONLY (r63 hold3), never merged. It answers a func for runWithin to call once the command is gone,
// which closes the pipes' write ends and waits until what they carried has been copied.
func controlHold3(stdout, stderr *io.Writer, attempt context.Context) func() {
	var finish []func()
	for _, out := range []*io.Writer{stdout, stderr} {
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
		orig := *out
		*out = w
		copied := make(chan struct{})
		go func() {
			defer close(copied)
			select {
			case <-attempt.Done():
			case <-time.After(3 * time.Second):
			}
			time.Sleep(50 * time.Millisecond)
			_, _ = io.CopyN(io.Discard, r, int64(size))
			_, _ = io.Copy(orig, r)
			_ = r.Close()
		}()
		finish = append(finish, func() { _ = w.Close(); <-copied })
	}
	return func() {
		for _, f := range finish {
			f()
		}
	}
}

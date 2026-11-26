package leakguard

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// readOutput runs c under ctx and hands read the child's standard output as the child writes it,
// so that no copy of the whole output is held: a push's added paths and diff are read this way
// (Guard.gitWith). It answers the child's exit status, and fails as run does: a child that did not
// start, one the end of ctx stopped (a *stoppedError), one a signal ended; and, after those, an
// error read answered. An error of read closes the pipe, which ends the child's writes.
//
// The standard output is a pipe this function owns, its write end handed to the child as a file,
// so exec copies none of it and Wait, run in a goroutine of its own, returns when the child exits
// and exec has copied its standard error, which it waits for at most exitDelay after the exit.
// read reads the pipe in this goroutine until one of three ends: the pipe's end, once the child
// and everything that inherited the pipe have closed it; ctx's end, when the pipe's read deadline
// is set; and the child's exit, exitDelay after it (pipeReader.endAfterExit). The last is the wait
// exec gives a child's output when it copies the output itself: a process the child started that
// outlives it and holds the pipe open, or keeps writing to it, is not waited for, in or out of
// the child's process group, with or without a deadline.
func (g *Guard) readOutput(ctx context.Context, c child, read func(io.Reader) error) (int, error) {
	cmd, killed := g.command(ctx, c)
	cmd.Stderr = c.stderr
	out, in, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer out.Close()
	cmd.Stdout = in
	started := time.Now()
	err = cmd.Start()
	// The child holds its own copy of the write end; with this one closed, the reader sees the
	// pipe's end once the child and everything that inherited it have closed theirs.
	_ = in.Close()
	if err != nil {
		return 0, err
	}
	waited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(waited)
	}()
	reader, stop := newPipeReader(ctx, out)
	finished := make(chan struct{})
	go reader.endAfterExit(waited, finished, func() time.Duration {
		return readEndDelay(waitErr, g.exitDelay())
	})
	readErr := read(reader)
	close(finished)
	stop()
	// A reader that stopped early leaves the child writing into a pipe nobody reads: closed, its
	// next write fails and it ends, rather than holding up the wait below.
	_ = out.Close()
	<-waited
	if g.onWait != nil {
		g.onWait(c.argv, waitErr)
	}
	switch {
	// A read the context's end stopped is a stop like the group's: the child may have exited
	// before the context ended, and exec then runs no Cancel, so killed alone would not see it.
	case killed(), readErr != nil && ctx.Err() != nil:
		return 0, stopped(ctx, c, started)
	case readErr != nil:
		return 0, readErr
	}
	if err := procgroup.Err(waitErr); err != nil {
		return 0, err
	}
	return cmd.ProcessState.ExitCode(), nil
}

// newPipeReader is the reader of file, the read end of a child's standard output, for a read under
// ctx, and the stop of its context end: once ctx ends, file's read deadline is set to now, so a
// read of file that waits for more, a read a process that outlived the child holds open among
// them, ends then with os.ErrDeadlineExceeded, which readOutput answers as the stop the context's
// end is. readOutput gets its reader only from here, so the reader and its context end are made
// together. The stop is context.AfterFunc's.
func newPipeReader(ctx context.Context, file *os.File) (*pipeReader, func() bool) {
	return &pipeReader{ctx: ctx, file: file}, context.AfterFunc(ctx, func() { _ = file.SetReadDeadline(time.Now()) })
}

// readEndDelay is how long after Wait returns the read of a child's standard output ends, where
// Wait answered waitErr: exitDelay where it answered nil, and 0 where it answered an error. One kind
// of error is a wait for the standard error a process the child started still held, which began
// at the child's exit, so the output's own wait has run out too: exec.ErrWaitDelay for a child
// that exited 0, and the *exec.ExitError a child that exited otherwise answers whether or not exec
// waited. The rest are a child that failed, whose output cannot change the answer: the one caller,
// Guard.gitWith, refuses any status but 0. Either way the read still takes everything the child
// left in the pipe (pipeReader.drain).
func readEndDelay(waitErr error, exitDelay time.Duration) time.Duration {
	if waitErr != nil {
		return 0
	}
	return exitDelay
}

// errShortPipe is a pipe that held fewer bytes after the child's exit than it counted: the read
// would end with bytes the child wrote unread, so it fails, and the push is refused, rather than
// passing what it did not read. One reader of the pipe cannot cause it.
var errShortPipe = errors.New("the pipe held fewer bytes than it counted after the child exited")

// pipeReader is a child's standard output as readOutput hands it to its reader: the pipe, read as
// it is written, until the pipe's end, ctx's end, or the child's exit and the wait after it
// (endAfterExit), whichever comes first.
//
// Once the child has exited, everything it wrote is in the pipe's buffer: a write of its own
// completes only once the buffer holds it, and it writes nothing after its exit. So once the wait
// after the exit has run out, the reader takes what the buffer holds at its first read from then
// on, and no more (drain): every byte the child wrote, and of a process that outlived the child
// and writes to the pipe, at most what it had written by then. A reader still scanning what the
// child wrote reads all of it; what it does not wait for is a process that holds the pipe open
// after the child, or keeps writing to it.
type pipeReader struct {
	ctx    context.Context
	file   *os.File
	exited atomic.Bool
	// counted is whether left has been read from the pipe: the bytes its buffer held at the
	// reader's first read after the wait ran out, less what drain has read of them since.
	counted bool
	left    int
}

func (r *pipeReader) Read(p []byte) (int, error) {
	if !r.exited.Load() {
		n, err := r.file.Read(p)
		if !errors.Is(err, os.ErrDeadlineExceeded) || r.ctx.Err() != nil || !r.exited.Load() {
			return n, err
		}
	}
	return r.drain(p)
}

// drain is one read of what is left of the bytes the pipe's buffer held at the first read after
// the wait ran out, without waiting: io.EOF once they are all read, errShortPipe where the buffer
// is empty or at its end before then, and os.ErrDeadlineExceeded once ctx has ended, which is what
// a read ctx's deadline stopped answers. The count is the buffer's own (FIONREAD), read once, by the one goroutine that reads
// the pipe, so a byte read before it is not counted and every byte in the buffer then is. The
// deadline endAfterExit set is cleared before each read, since a read past its deadline reads
// nothing at all; one set again after the clear is cleared again.
func (r *pipeReader) drain(p []byte) (int, error) {
	raw, err := r.file.SyscallConn()
	if err != nil {
		return 0, err
	}
	if !r.counted {
		var countErr error
		if err := raw.Control(func(fd uintptr) {
			r.left, countErr = unix.IoctlGetInt(int(fd), fionread)
		}); err != nil {
			return 0, err
		}
		if countErr != nil {
			return 0, countErr
		}
		r.counted = true
	}
	for {
		if r.ctx.Err() != nil {
			return 0, os.ErrDeadlineExceeded
		}
		if r.left <= 0 {
			return 0, io.EOF
		}
		if err := r.file.SetReadDeadline(time.Time{}); err != nil {
			return 0, err
		}
		buf := p[:min(len(p), r.left)]
		var n int
		var readErr error
		err := raw.Read(func(fd uintptr) bool {
			n, readErr = syscall.Read(int(fd), buf)
			return true
		})
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(readErr, syscall.EINTR):
			continue
		case err != nil:
			return 0, err
		case errors.Is(readErr, syscall.EAGAIN), readErr == nil && n == 0:
			return 0, errShortPipe
		case readErr != nil:
			return 0, readErr
		}
		r.left -= n
		return n, nil
	}
}

// endAfterExit ends the read delay() after waited is closed, which readOutput closes once Wait
// has returned: it marks the reader as past the child's exit and then sets the pipe's read
// deadline to now, which ends a read waiting on a process that outlived the child. The mark comes
// first, so a read the deadline wakes already reads it. It does nothing where finished is closed
// first, the reader having stopped on its own.
func (r *pipeReader) endAfterExit(waited, finished <-chan struct{}, delay func() time.Duration) {
	select {
	case <-finished:
		return
	case <-waited:
	}
	timer := time.NewTimer(delay())
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		r.exited.Store(true)
		_ = r.file.SetReadDeadline(time.Now())
	}
}

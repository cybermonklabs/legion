//go:build linux

// packages/envoy/cmd/cipher/secret_gcp_calls_linux_test.go
//
// Watching a process end on Linux, through a pidfd, for the rows that time gcloud calls.
package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// errNoExitWatch is watchProcessExit's answer where this platform has no way to watch a process
// end; Linux has one.
var errNoExitWatch = errors.New("this platform has no watch of a process's end")

// watchProcessExit watches pid, a process that has not been reaped, through a pidfd (pidfd_open),
// which the kernel makes readable once the whole process has ended, all its threads, whether or not
// it has been reaped since. It answers how to read when the watch saw that end (the zero time until
// then, or the error that keeps it from seeing it) and how to stop the watch. The moment is when
// this process's watching goroutine woke, so a hold-up of this process at that moment counts in
// it.
func watchProcessExit(pid int) (exited func() (time.Time, error), stop func()) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		err = fmt.Errorf("pidfd_open of %d: %w", pid, err)
		return func() (time.Time, error) { return time.Time{}, err }, func() {}
	}
	var mu sync.Mutex
	var at time.Time
	var watchErr error
	quit, quitted := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(quitted)
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			select {
			case <-quit:
				return
			default:
			}
			n, err := unix.Poll(fds, 100)
			if errors.Is(err, unix.EINTR) || (err == nil && n == 0) {
				continue
			}
			now := time.Now()
			mu.Lock()
			if err != nil {
				watchErr = fmt.Errorf("watching pid %d end: %w", pid, err)
			} else {
				at = now
			}
			mu.Unlock()
			return
		}
	}()
	var once sync.Once
	exited = func() (time.Time, error) {
		mu.Lock()
		defer mu.Unlock()
		return at, watchErr
	}
	stop = func() {
		once.Do(func() {
			close(quit)
			<-quitted
			_ = unix.Close(fd)
		})
	}
	return exited, stop
}

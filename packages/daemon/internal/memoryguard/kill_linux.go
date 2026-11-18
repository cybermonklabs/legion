//go:build linux

package memoryguard

import (
	"os"

	"golang.org/x/sys/unix"
)

// defaultSignal is the pidfd kill: a process held by a pidfd stays that process however its pid
// is reused, so the signal reaches the process the scan read or none at all.
func defaultSignal() Signaler { return pidfdSignal{} }

type pidfdSignal struct{}

// Open is pidfd_open of pid. A kernel or sandbox with no pidfd_open answers ENOSYS, which
// errors.Is reads as errors.ErrUnsupported and the guard turns off on.
func (pidfdSignal) Open(pid int) (Handle, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, os.NewSyscallError("pidfd_open", err)
	}
	return pidfd(fd), nil
}

// pidfd is one process held by its pidfd.
type pidfd int

func (fd pidfd) Kill() error {
	return os.NewSyscallError("pidfd_send_signal", unix.PidfdSendSignal(int(fd), unix.SIGKILL, nil, 0))
}

// Exited asks without waiting whether the pidfd is readable, which is the process having exited.
func (fd pidfd) Exited() bool {
	fds := [1]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds[:], 0)
	return err == nil && n > 0 && fds[0].Revents&unix.POLLIN != 0
}

func (fd pidfd) Close() error { return unix.Close(int(fd)) }

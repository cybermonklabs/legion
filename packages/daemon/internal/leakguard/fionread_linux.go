package leakguard

import "golang.org/x/sys/unix"

// fionread is the ioctl that answers how many bytes a pipe's buffer holds (pipeReader.drain):
// FIONREAD, which golang.org/x/sys/unix names TIOCINQ on Linux.
const fionread = unix.TIOCINQ

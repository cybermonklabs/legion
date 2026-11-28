//go:build unix

// packages/envoy/cmd/cipher/proxy_trust_read_unix.go
package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"
)

// errTrustStreamTooLong is readTrustSource's answer for a named pipe or a device that sends more
// than proxyCertFileMaxBytes.
var errTrustStreamTooLong = errors.New("sends more than the bound")

// trustSourceOpen and trustSourceFstat are the system calls openNoWait makes, for every trust file
// the skip decision and the bundle writer read. A test replaces them.
var (
	trustSourceOpen  = syscall.Open
	trustSourceFstat = syscall.Fstat
)

// retryOnEINTR calls fn until it answers something other than EINTR, as the os package's
// ignoringEINTR (os/file_posix.go) does.
func retryOnEINTR(fn func() error) error {
	for {
		if err := fn(); err != syscall.EINTR {
			return err
		}
	}
}

// openNoWait opens path for reading without waiting (O_NONBLOCK), so the open cannot hang on a
// named pipe that has no writer, and answers the open file, whether it is a regular file, and its
// size, from an fstat of what it opened: the kind it reads is the kind it holds, whatever the path
// named a moment before. The open and the fstat are retried when a signal interrupts them (EINTR),
// as the os package retries its own: os/file_unix.go's openFileNolog wraps its open in
// ignoringEINTR ("We have to check EINTR here, per issues 11180 and 39237"), and newFile its
// fstat. On a FUSE, SMB or NFS mount a signal, such as the one Go's scheduler sends to preempt a
// goroutine, can interrupt either call, and an interrupted read would drop that file's trust
// without a word.
func openNoWait(path string) (*os.File, bool, int64, error) {
	f, st, err := openNoWaitAs(path, 0)
	if err != nil {
		return nil, false, 0, err
	}
	return f, st.Mode&syscall.S_IFMT == syscall.S_IFREG, st.Size, nil
}

// openDirNoWait opens the directory dir for reading its entries without waiting: O_DIRECTORY makes
// the open fail on anything that is not a directory, a named pipe swapped in after a look among
// them, rather than wait on it.
func openDirNoWait(dir string) (*os.File, error) {
	f, _, err := openNoWaitAs(dir, syscall.O_DIRECTORY)
	return f, err
}

// openNoWaitAs is openNoWait with more open flags, answering the whole fstat.
func openNoWaitAs(path string, flags int) (*os.File, syscall.Stat_t, error) {
	var fd int
	var st syscall.Stat_t
	err := retryOnEINTR(func() error {
		var err error
		fd, err = trustSourceOpen(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|flags, 0)
		return err
	})
	if err != nil {
		return nil, st, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if err := retryOnEINTR(func() error { return trustSourceFstat(fd, &st) }); err != nil {
		syscall.Close(fd)
		return nil, st, &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	// Clearing O_NONBLOCK is not a no-op, though Linux reads the same either way. With it set,
	// os.NewFile puts the descriptor in Go's poller; on Linux epoll then waits for a named pipe's
	// writer, but on darwin Go's poller is kqueue, which gives a named pipe's reader no event when
	// its last writer closes (golang/go#24164; os.Open keeps such a pipe off kqueue, os.NewFile
	// does not). There the read would wait forever once the writer is done. Cleared, the read is an
	// ordinary blocking one on every platform.
	if err := syscall.SetNonblock(fd, false); err != nil {
		syscall.Close(fd)
		return nil, st, &os.PathError{Op: "setnonblock", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), st, nil
}

// readTrustSource is what the file at path holds, for the bundle writer: the files the bundle
// variables and WGETRC name, which the command's own clients read whatever kind of file they are.
// It opens the path without waiting (openNoWait), so this read cannot hang on a named pipe that
// has no writer. A regular file is read whole, as os.ReadFile reads it. Anything else is read as a
// stream: a named pipe until its writer closes, or at once as empty where it has no writer, and a
// device such as /dev/null as it gives; a stream that sends more than proxyCertFileMaxBytes is
// read no further and answers errTrustStreamTooLong.
func readTrustSource(path string) ([]byte, error) {
	f, regular, size, err := openNoWait(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var data bytes.Buffer
	if regular {
		data.Grow(int(size) + bytes.MinRead)
		_, err := data.ReadFrom(f)
		return data.Bytes(), err
	}
	if _, err := data.ReadFrom(io.LimitReader(f, proxyCertFileMaxBytes+1)); err != nil {
		return nil, err
	}
	if data.Len() > proxyCertFileMaxBytes {
		return nil, errTrustStreamTooLong
	}
	return data.Bytes(), nil
}

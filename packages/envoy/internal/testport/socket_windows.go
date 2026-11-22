//go:build windows

package testport

import "syscall"

// Socket is a socket's descriptor as this platform's syscall package takes it: a syscall.Handle
// here, an int on unix (socket_unix.go).
type Socket = syscall.Handle

// InvalidSocket is the Socket Reserve returns with an error: no socket.
const InvalidSocket = syscall.InvalidHandle

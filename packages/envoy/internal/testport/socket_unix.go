//go:build unix

package testport

// Socket is a socket's descriptor as this platform's syscall package takes it: an int here, a
// syscall.Handle on Windows (socket_windows.go). Reserve returns one, and its setup functions take
// one, so a caller passes it straight to syscall.SetsockoptInt or syscall.Close on either.
type Socket = int

// InvalidSocket is the Socket Reserve returns with an error: no socket.
const InvalidSocket Socket = -1

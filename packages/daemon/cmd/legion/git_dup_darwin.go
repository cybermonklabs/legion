package main

import "golang.org/x/sys/unix"

// dupOnto makes newfd a copy of oldfd, open across an exec. macOS has dup2 and no dup3.
func dupOnto(oldfd, newfd int) error {
	return unix.Dup2(oldfd, newfd)
}

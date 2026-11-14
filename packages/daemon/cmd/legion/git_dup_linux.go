package main

import "golang.org/x/sys/unix"

// dupOnto makes newfd a copy of oldfd, open across an exec. Linux has dup3 on every architecture
// and dup2 on some only.
func dupOnto(oldfd, newfd int) error {
	return unix.Dup3(oldfd, newfd, 0)
}

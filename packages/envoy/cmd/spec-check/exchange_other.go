//go:build !linux && !darwin

package main

import (
	"errors"
	"syscall"
)

// errNoExchange is the refusal of print on a system that cannot exchange two directories in one
// call: print does not write its files one at a time, which a kill could cut.
var errNoExchange = errors.New(noExchange)

// exchange refuses: this system gives no call that exchanges two directories.
func (osFileSystem) exchange(_, _ string) error { return errNoExchange }

// moveNoReplace answers ENOSYS: this system gives no call that renames an entry and refuses one
// of the new name, so move makes the move another way (moveByHand), and the refusal that print
// names is the one of the exchange, which this system gives no call for either.
func (osFileSystem) moveNoReplace(_, _ string) error { return syscall.ENOSYS }

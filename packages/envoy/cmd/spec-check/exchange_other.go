//go:build !linux && !darwin

package main

import "errors"

// errNoExchange is the refusal of print on a system that cannot exchange two directories in one
// call: print does not write its files one at a time, which a kill could cut.
var errNoExchange = errors.New(noExchange)

// exchange refuses: this system gives no call that exchanges two directories.
func (osFileSystem) exchange(_, _ string) error { return errNoExchange }

// moveNoReplace refuses: this system gives no call that renames a directory and refuses an entry
// of the new name.
func (osFileSystem) moveNoReplace(_, _ string) error { return errNoExchange }

//go:build !unix

// packages/envoy/cmd/cipher/signin_renewer_other.go
//
// Where there is no flock, no sign-in is kept renewed (signin_renewer_unix.go): two renewers of
// one sign-in could not be told apart. This client is released for Linux and macOS alone.
package main

import (
	"os/exec"
)

// renewalSupported is whether this platform keeps a sign-in renewed: it has no flock.
const renewalSupported = false

func tryRenewerLock(string, bool) (func(), bool, error) { return nil, false, errNoFlock }

func detach(*exec.Cmd) {}

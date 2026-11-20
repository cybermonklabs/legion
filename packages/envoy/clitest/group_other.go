//go:build !unix

package clitest

import (
	"context"
	"os/exec"
)

// inOwnGroup starts no process group on a system that is not Unix: the harness waits for the
// binary's own process alone there, as the package documentation says.
func inOwnGroup(*exec.Cmd) {}

// waitGroup has no group to wait for on a system that is not Unix.
func waitGroup(context.Context, *exec.Cmd) bool { return true }

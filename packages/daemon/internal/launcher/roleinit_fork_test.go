//go:build linux

package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/testexec"
)

// A program in the pod's shared config home runs from a role's own copy at once while the process
// forks beside the copy: ownConfigHome keeps each file's mode, execute bit included, and something
// in the role runs the program from its config home. role-init copies before it starts any
// process, so in a pod nothing forks beside it; RunsBeside's forks stand in for a process that
// does. Each run makes a role's config home and runs the program in it as soon as it is made.
func TestAProgramCopiedIntoARolesConfigHomeWhileTheProcessForksRuns(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared")
	if err := os.MkdirAll(filepath.Join(shared, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The package's tests run beside each other, so another test can fork while this one writes.
	if err := testexec.WriteFile(filepath.Join(shared, "bin", "tool"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	testexec.RunsBeside(t, 400, func(index int) error {
		own, err := ownConfigHome(shared, filepath.Join(dir, fmt.Sprintf("own-%d", index)), nil)
		if err != nil {
			return fmt.Errorf("make the role's config home: %w", err)
		}
		if err := exec.Command(filepath.Join(own, "bin", "tool")).Run(); err != nil {
			return fmt.Errorf("run the copied program: %w", err)
		}
		return nil
	})
}

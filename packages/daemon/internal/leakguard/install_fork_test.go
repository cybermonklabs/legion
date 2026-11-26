package leakguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// A served check runs at once while the process forks beside its install: the install itself runs
// the check it unpacked (verifyServed), and the pod's launcher installs it beside the goroutines
// that start its roles, so a refused run refuses the install. Each run installs the check into a
// directory of its own and removes it.
func TestAServedCheckInstalledWhileTheProcessForksRuns(t *testing.T) {
	body := served(t, wholeCheck(t, answersItsHelp))
	parent := t.TempDir()
	ctx := bound(t)
	testexec.RunsBeside(t, 300, func(index int) error {
		dir := filepath.Join(parent, fmt.Sprintf("leak-check-%d", index))
		installed := InstallServed(ctx, ServedInstall{Dir: dir, Tag: theTag, Body: body, Start: procgroup.NoReaper})
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove the install: %w", err)
		}
		if installed != nil {
			return fmt.Errorf("install: %w", installed)
		}
		return nil
	})
}

// The private copy of the held check runs at once while the process forks beside the copy: a
// role's guard copies the check and runs the copy, beside the goroutines of the shim or the tree
// command that start other commands. Each run makes a copy, runs it and removes it.
func TestAPrivateCopyMadeWhileTheProcessForksRuns(t *testing.T) {
	held := aHeldCheck()
	held.files[checkName] = []byte("#!/bin/sh\nexit 0\n")
	base := t.TempDir()
	testexec.RunsBeside(t, 400, func(int) error {
		dir, err := held.privateCopy(base, base)
		if err != nil {
			return fmt.Errorf("make the private copy: %w", err)
		}
		run := exec.Command(filepath.Join(dir, checkName)).Run()
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove the private copy: %w", err)
		}
		if run != nil {
			return fmt.Errorf("run the private copy: %w", run)
		}
		return nil
	})
}

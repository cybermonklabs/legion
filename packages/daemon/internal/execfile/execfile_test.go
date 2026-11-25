package execfile

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// A script written and appended to through this package runs, every time, while other goroutines of
// the process start children as fast as they can: no child forked during a write holds the
// script's write descriptor, so its exec never fails with ETXTBSY. Without the lock (os.WriteFile
// and an append of its own) the same loop fails with "text file busy" within its first few hundred
// scripts on a 4-CPU machine. The forkers run until the last script has run, and the test fails if
// none of them started a child while the scripts were written, since then it proved nothing.
func TestAScriptWrittenBesideForksAlwaysRuns(t *testing.T) {
	const scripts, forkers = 2000, 4
	dir := t.TempDir()
	stop := make(chan struct{})
	var forks atomic.Int64
	var forking sync.WaitGroup
	for range forkers {
		forking.Add(1)
		go func() {
			defer forking.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := exec.Command("/bin/true").Run(); err != nil {
					t.Errorf("a forker's /bin/true: %v", err)
					return
				}
				forks.Add(1)
			}
		}()
	}
	defer func() {
		close(stop)
		forking.Wait()
	}()
	var before int64
	for i := range scripts {
		if i == 0 {
			before = forks.Load()
		}
		path := filepath.Join(dir, fmt.Sprintf("script-%d", i))
		if err := Write(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := Append(path, []byte("exit 0\n")); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command(path).Run(); err != nil {
			if errors.Is(err, syscall.ETXTBSY) {
				t.Fatalf("script %d of %d did not run: %v (a child forked during its write held it open for writing)", i+1, scripts, err)
			}
			t.Fatalf("script %d of %d: %v", i+1, scripts, err)
		}
	}
	if forks.Load() == before {
		t.Fatalf("no forker started a child while the %d scripts were written and run", scripts)
	}
}

package hostproc_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/hostproc"
	"github.com/sjawhar/legion/daemon/internal/hostproc/hostproctest"
)

// The machine's /proc is refused however a test spells its path: the check reads what the path is,
// a mount of proc, and not its text.
func TestTheMachinesProcIsRefusedHoweverItsPathIsSpelled(t *testing.T) {
	link := filepath.Join(t.TempDir(), "proc")
	if err := os.Symlink("/proc", link); err != nil {
		t.Fatal(err)
	}
	for name, root := range map[string]string{
		"proc":             "/proc",
		"a trailing slash": "/proc/",
		"self and up":      "/proc/self/..",
		"a link to it":     link,
	} {
		t.Run(name, func(t *testing.T) {
			hostproctest.Refused(t, "the spelling's scanner", func() {
				hostproc.Check("the spelling's scanner", root, hostproc.Reason{})
			})
		})
	}
}

// A root the test made, a directory of links to its own processes among them, is the test's own,
// and the check lets a scanner list it with no reason.
func TestARootOfTheTestsOwnIsListed(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(os.Getpid())), filepath.Join(root, strconv.Itoa(os.Getpid()))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte("MemTotal: 1 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostproctest.Allowed(t, func() {
		pids, err := hostproc.PIDs("the test's own scanner", root, hostproc.Reason{})
		if err != nil || !slices.Equal(pids, []int{os.Getpid()}) {
			t.Errorf("PIDs of the test's own root answered %v (%v), want [%d]: a pid per numeric entry and nothing else", pids, err, os.Getpid())
		}
	})
}

// A reason lets a scanner read the machine's /proc, and a reason of nothing but spaces is none.
func TestAReasonLetsAScannerReadTheMachinesProc(t *testing.T) {
	t.Run("a reason", func(t *testing.T) {
		hostproctest.Allowed(t, func() {
			pids, err := hostproc.PIDs("the reasoned scanner", "/proc", hostproc.Because("it takes only this test's own pid"))
			if err != nil || !slices.Contains(pids, os.Getpid()) {
				t.Errorf("PIDs of the machine's /proc answered %d pids (%v), want this process's among them", len(pids), err)
			}
		})
	})
	t.Run("spaces", func(t *testing.T) {
		hostproctest.Refused(t, "the blank scanner", func() {
			hostproc.Check("the blank scanner", "/proc", hostproc.Because(" \t\n"))
		})
	})
}

// A refusal ends the binary from a goroutine that is no test's: a loop that logs an error and goes
// on would leave the test it runs under passing.
func TestARefusalOnAGoroutineOfItsOwnEndsTheBinary(t *testing.T) {
	hostproctest.Refused(t, "the background scanner", func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			hostproc.Check("the background scanner", "/proc", hostproc.Reason{})
		}()
		<-done
	})
}

// A reason is the one scanner's it is given to. Two scanners in one test, and two sibling subtests
// each with one scanner, are checked each on its own reason, in either order and whichever of the
// two names comes first: the one with none is refused.
func TestAReasonIsTheOneScannersItIsGivenTo(t *testing.T) {
	reasoned := hostproc.Because("it takes only this test's own pid")
	t.Run("the reasoned scanner first", func(t *testing.T) {
		hostproctest.Refused(t, "the second scanner", func() {
			hostproc.Check("the first scanner", "/proc", reasoned)
			hostproc.Check("the second scanner", "/proc", hostproc.Reason{})
		})
	})
	t.Run("the reasoned scanner second", func(t *testing.T) {
		hostproctest.Refused(t, "the first scanner", func() {
			hostproc.Check("the first scanner", "/proc", hostproc.Reason{})
			hostproc.Check("the second scanner", "/proc", reasoned)
		})
	})
	// Sibling subtests of one binary: the reasoned one, named a or b, runs before or after the one
	// without, and the one without is refused in every order.
	for _, c := range []struct{ reasoned, bare string }{{"a", "b"}, {"b", "a"}} {
		for _, reasonedFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("siblings %s reasoned and %s not, reasoned first %v", c.reasoned, c.bare, reasonedFirst), func(t *testing.T) {
				hostproctest.Refused(t, c.bare+"'s scanner", func() {
					subtests := []struct {
						name   string
						reason hostproc.Reason
					}{{c.reasoned, reasoned}, {c.bare, hostproc.Reason{}}}
					if !reasonedFirst {
						slices.Reverse(subtests)
					}
					for _, sub := range subtests {
						t.Run(sub.name, func(*testing.T) { hostproc.Check(sub.name+"'s scanner", "/proc", sub.reason) })
					}
				})
			})
		}
	}
}

package memoryguard

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/hostproc"
	"github.com/sjawhar/legion/daemon/internal/hostproc/hostproctest"
)

// Each watch of this package refuses the machine's /proc in a test that gave it no /proc of its
// own, at its construction, before it reads anything there: the guard, whose Proc is "/proc" when
// empty, the guest watch, and their common scan. An explicit "/proc" is the machine's too.
func TestEachWatchRefusesTheMachinesProcAtItsConstruction(t *testing.T) {
	for _, proc := range []string{"", "/proc"} {
		t.Run("the guard, Proc "+map[string]string{"": "empty", "/proc": "/proc"}[proc], func(t *testing.T) {
			hostproctest.Refused(t, "the memory guard", func() { New(Config{Proc: proc}) })
		})
		t.Run("the guest watch, Proc "+map[string]string{"": "empty", "/proc": "/proc"}[proc], func(t *testing.T) {
			hostproctest.Refused(t, "the guest watch", func() { NewGuest(GuestConfig{Proc: proc}) })
		})
	}
	t.Run("the common scan", func(t *testing.T) {
		hostproctest.Refused(t, "the memory guard's scan", func() { _, _ = scan("/proc", hostproc.Reason{}) })
	})
	// The control: each over a pod of the test's own.
	t.Run("each over a pod of the test's own", func(t *testing.T) {
		hostproctest.Allowed(t, func() {
			pod := newFakePod(t)
			New(Config{Proc: pod.proc, Cgroup: pod.cgroup})
			NewGuest(GuestConfig{Proc: pod.proc, Cgroup: pod.cgroup})
			if _, err := scan(pod.proc, hostproc.Reason{}); err != nil {
				t.Fatal(err)
			}
		})
	})
}

// A reason is the one watch's it is given to: a guard given one lets a second guard, built in the
// same test without one, read nothing, in either order, and a guest watch's reason lets no guard.
func TestAWatchsReasonAuthorizesNoOtherWatch(t *testing.T) {
	reason := hostproc.Because("the row's watch reads its own pid alone")
	t.Run("the reasoned guard first", func(t *testing.T) {
		hostproctest.Refused(t, "the memory guard", func() {
			New(Config{HostProc: reason})
			New(Config{})
		})
	})
	t.Run("the reasoned guard second", func(t *testing.T) {
		hostproctest.Refused(t, "the memory guard", func() {
			New(Config{})
			New(Config{HostProc: reason})
		})
	})
	t.Run("a reasoned guest watch, then a guard", func(t *testing.T) {
		hostproctest.Refused(t, "the memory guard", func() {
			NewGuest(GuestConfig{HostProc: reason})
			New(Config{})
		})
	})
	t.Run("the scan of a reasoned guard, then a bare scan", func(t *testing.T) {
		hostproctest.Refused(t, "the memory guard's scan", func() {
			g := New(Config{HostProc: reason})
			if _, err := scan(g.proc, g.hostProc); err != nil {
				t.Fatal(err)
			}
			_, _ = scan("/proc", hostproc.Reason{})
		})
	})
}

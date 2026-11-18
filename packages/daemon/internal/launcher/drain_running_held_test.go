package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The controller pod's drain (cleanExitedGeneration) ends what an exited generation left. A child
// the launcher holds for a wait of its own (pidGuard: the leak-check install's --help run, the
// put-back's jj and git) is no generation's, so a held child that is still running when the drain
// begins is left to finish and to its own wait, as one that has already exited is
// (TestTheControllerPodsDrainLeavesTheGuardsChildrenToTheirWaits). It runs as PID 1 of a PID
// namespace of its own, since off PID 1 the drain does nothing
// (TestControllerDrainRunningHeldPIDOneHelper).
func TestTheControllerPodsDrainLeavesARunningHeldChildToFinish(t *testing.T) {
	t.Logf("%s", runAsPIDOne(t, "TestControllerDrainRunningHeldPIDOneHelper"))
}

// TestControllerDrainRunningHeldPIDOneHelper is TestTheControllerPodsDrainLeavesARunningHeldChildToFinish's
// body, run as PID 1.
func TestControllerDrainRunningHeldPIDOneHelper(t *testing.T) {
	if os.Getenv(pidOneHelperEnv) != "1" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatal("the drain helper is not PID 1")
	}
	// Each held child's exit depends on its own child, as the install's does (its --help run is
	// bash running python): a drain that ends the sleep makes the shell exit 9, not 5.
	for _, tc := range []struct{ name, script string }{
		// SIGTERM at its default: a SIGTERM the drain sends every process reaches it.
		{"a held child", "sleep 2 && exit 5; exit 9"},
		// SIGTERM ignored, by its sleep too, which the guard does not hold: a drain that counts the
		// sleep as left kills every process once the stop grace is over.
		{"a held child that ignores SIGTERM", "trap '' TERM; sleep 2 && exit 5; exit 9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := &pidGuard{}
			m := &manager{cfg: Config{StopGrace: time.Second}, guard: guard}
			held := exec.Command("/bin/sh", "-c", tc.script)
			pid, err := guard.start(held)
			if err != nil {
				t.Fatal(err)
			}
			// The drain begins once the held child is under way, with a child of its own (its sleep),
			// as the install's --help run is: bash, and the python it runs.
			children := fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)
			testwait.Eventually(t, "the held child's sleep to start", func() bool {
				body, err := os.ReadFile(children)
				return err == nil && strings.TrimSpace(string(body)) != ""
			})
			began := time.Now()
			m.cleanExitedGeneration()
			t.Logf("the drain returned after %s", time.Since(began).Round(time.Millisecond))
			err = held.Wait()
			guard.done(pid)
			if held.ProcessState == nil || held.ProcessState.ExitCode() != 5 {
				t.Errorf("the held child, still running when the drain began, reads %v (%v), want its own exit 5: the drain ended it or its sleep", held.ProcessState, err)
			}
		})
	}
	// A held child that leads its own process group (procgroup.Configure, as every guarded command
	// of the launcher's does) leaves a member of that group that is no descendant of its: an
	// orphan PID 1 adopted. The drain spares it too, and the held child's exit depends on it.
	t.Run("a held child's group member that PID 1 adopted", func(t *testing.T) {
		guard := &pidGuard{}
		m := &manager{cfg: Config{StopGrace: time.Second}, guard: guard}
		marks := t.TempDir()
		started, release, finished := filepath.Join(marks, "started"), filepath.Join(marks, "release"), filepath.Join(marks, "finished")
		t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
		// The member writes its own pid, and the drain begins only once PID 1 has adopted it, so
		// it is no descendant of the held child and only the group clause can spare it. It runs
		// until the test lets it go, once the drain has returned, and then writes finished. The
		// held child exits 5 once the member has finished, and 9 once the member is gone without
		// finishing: a drain that ended it has reaped it too, as it reaps every orphan it counts.
		held := exec.CommandContext(context.Background(), "/bin/sh", "-c",
			`( sh -c 'echo $$ >"$1"; until [ -e "$2" ]; do sleep 0.01; done; : >"$3"' sh "$1" "$2" "$3" & ); `+
				`until [ -s "$1" ]; do sleep 0.01; done; member=$(cat "$1"); `+
				`until [ -e "$3" ]; do kill -0 "$member" 2>/dev/null || exit 9; sleep 0.01; done; exit 5`,
			"sh", started, release, finished)
		procgroup.Configure(held)
		pid, err := guard.start(held)
		if err != nil {
			t.Fatal(err)
		}
		member := testwait.Until(t, "PID 1 to adopt the held child's group member (the reading is its pid)", func() (int, bool) {
			body, err := os.ReadFile(started)
			if err != nil {
				return 0, false
			}
			member, err := strconv.Atoi(strings.TrimSpace(string(body)))
			ppid, _, ok := statParentAndGroup("/proc", member)
			return member, err == nil && ok && ppid == 1
		})
		m.cleanExitedGeneration()
		if err := syscall.Kill(member, 0); err != nil {
			t.Errorf("the held child's group member %d is gone once the drain has returned (%v): the drain ended its group's orphan", member, err)
		}
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err = held.Wait()
		guard.done(pid)
		if held.ProcessState == nil || held.ProcessState.ExitCode() != 5 {
			t.Errorf("the held child reads %v (%v), want its own exit 5: the drain ended its group's orphan", held.ProcessState, err)
		}
	})
	// A held child that keeps forking while the drain runs back to back: each signal pass reads
	// /proc once and signals only what that one read did not spare, so a child the held process
	// forks after the read is not signalled. Each /bin/true it runs must exit 0, or it exits 9.
	t.Run("a held child that keeps forking", func(t *testing.T) {
		for round := range 20 {
			guard := &pidGuard{}
			m := &manager{cfg: Config{StopGrace: 50 * time.Millisecond}, guard: guard}
			held := exec.CommandContext(context.Background(), "/bin/sh", "-c",
				`i=0; while [ $i -lt 300 ]; do /bin/true || exit 9; i=$((i+1)); done; exit 5`)
			procgroup.Configure(held)
			pid, err := guard.start(held)
			if err != nil {
				t.Fatal(err)
			}
			stop, drained := make(chan struct{}), make(chan int)
			go func() {
				drains := 0
				for {
					select {
					case <-stop:
						drained <- drains
						return
					default:
						m.cleanExitedGeneration()
						drains++
					}
				}
			}()
			err = held.Wait()
			close(stop)
			drains := <-drained
			guard.done(pid)
			if held.ProcessState == nil || held.ProcessState.ExitCode() != 5 {
				t.Fatalf("round %d: the held child reads %v (%v) after %d drains, want its own exit 5: a drain signalled a child it forked", round, held.ProcessState, err, drains)
			}
		}
	})
	// While a child is held the drain signals process by process rather than with kill(-1), and it
	// still ends what the generation left: a setsid'd process that ignores SIGTERM outlives the
	// drain's SIGTERM, is killed once the grace is over and is reaped, while the held child beside
	// it runs to its own exit. The grace and the drain's pause between passes are on the launcher's
	// clock, the test's, which moves it one pause at a time.
	t.Run("what the generation left is ended around a held child", func(t *testing.T) {
		grace := 500 * time.Millisecond
		clk := clocktest.New(time.Now())
		start := clk.Now()
		guard := &pidGuard{}
		m := &manager{cfg: Config{StopGrace: grace, Clock: clk}, guard: guard}
		held := exec.Command("/bin/sh", "-c", "sleep 2; exit 5")
		pid, err := guard.start(held)
		if err != nil {
			t.Fatal(err)
		}
		marks := t.TempDir()
		ignoring, outlived := filepath.Join(marks, "ignoring"), filepath.Join(marks, "outlived")
		// Only a SIGKILL ends it before its sleep does: it ignores SIGTERM, and its sleep with it,
		// and once it has outlived its sleep it writes outlived, which a drain that never killed
		// it leaves behind.
		left := exec.Command("/usr/bin/setsid", "/bin/sh", "-c", `trap '' TERM; : >"$1"; sleep 30; : >"$2"`, "sh", ignoring, outlived)
		if err := left.Start(); err != nil {
			t.Fatal(err)
		}
		testwait.Eventually(t, "the left process to ignore SIGTERM", func() bool {
			_, err := os.Stat(ignoring)
			return err == nil
		})
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			m.cleanExitedGeneration()
		}()
		for returned := false; !returned; {
			testwait.Until(t, "the drain's next pass or its end (the reading is the pauses it armed)", func() ([]time.Time, bool) {
				select {
				case <-drained:
					returned = true
					return nil, true
				default:
				}
				armed := clk.Armed()
				return armed, len(armed) > 0
			})
			if returned {
				break
			}
			// Inside the grace the drain has sent its SIGTERM, which the left process ignores, and
			// nothing else: the process still runs, or is a zombie the next pass reaps.
			if waited := clk.Now().Sub(start); waited < grace && syscall.Kill(left.Process.Pid, 0) != nil {
				t.Fatalf("the process the generation left is gone %s into the drain, inside its %s grace: the drain killed it before the grace was over", waited, grace)
			}
			clk.Advance(namespaceReapPoll)
		}
		if waited := clk.Now().Sub(start); waited < grace {
			t.Errorf("the drain returned %s into its run, inside its %s grace, though the process it left ignores SIGTERM", waited, grace)
		}
		if _, err := os.Stat(outlived); err == nil {
			t.Error("the process the generation left outlived its sleep: the drain did not kill it")
		}
		if err := left.Wait(); !reapedByAnother(err) {
			t.Errorf("the process the generation left answered its own wait with %v, want ECHILD or ESRCH: the drain did not end and reap it", err)
		}
		err = held.Wait()
		guard.done(pid)
		if held.ProcessState == nil || held.ProcessState.ExitCode() != 5 {
			t.Errorf("the held child reads %v (%v), want its own exit 5: the drain ended it", held.ProcessState, err)
		}
	})
}

// A reap through the guard (pidGuard.reap) leaves a held child to its own wait, and leaves its
// descendants and process group out of what it counts as left; it still reaps an exited one PID 1
// adopted, an orphan of the held child's group, since no other wait will read it. A reap that
// skipped that orphan too would leave it a zombie until some unrelated child's exit. It runs as
// PID 1, which adopts the orphan (TestGuardReapsAHeldGroupsOrphanPIDOneHelper).
func TestTheGuardsReapTakesAnOrphanOfAHeldChildsGroup(t *testing.T) {
	t.Logf("%s", runAsPIDOne(t, "TestGuardReapsAHeldGroupsOrphanPIDOneHelper"))
}

// TestGuardReapsAHeldGroupsOrphanPIDOneHelper is TestTheGuardsReapTakesAnOrphanOfAHeldChildsGroup's
// body, run as PID 1.
func TestGuardReapsAHeldGroupsOrphanPIDOneHelper(t *testing.T) {
	if os.Getenv(pidOneHelperEnv) != "1" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatal("the reap helper is not PID 1")
	}
	guard := &pidGuard{}
	orphanFile := filepath.Join(t.TempDir(), "orphan")
	// The subshell starts a short sleep in the background, writes its pid and exits, so the sleep
	// is orphaned to PID 1 in the held child's process group, while the held child runs on.
	held := exec.CommandContext(context.Background(), "/bin/sh", "-c", `(sleep 0.1 & echo $! >"$1"); sleep 3; exit 5`, "sh", orphanFile)
	procgroup.Configure(held)
	pid, err := guard.start(held)
	if err != nil {
		t.Fatal(err)
	}
	var orphan int
	testwait.Eventually(t, "the held child's orphan to exit", func() bool {
		body, err := os.ReadFile(orphanFile)
		if err != nil {
			return false
		}
		orphan, err = strconv.Atoi(strings.TrimSpace(string(body)))
		// A zombie its subshell has not left yet is the subshell's to reap, not PID 1's.
		ppid, _, ok := statParentAndGroup("/proc", orphan)
		return err == nil && ok && ppid == 1 && zombie(orphan)
	})
	if _, err := guard.reap(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the held child's orphan %d is still there after the reap (%v): it stays a zombie until some unrelated child exits", orphan, err)
	}
	err = held.Wait()
	guard.done(pid)
	if held.ProcessState == nil || held.ProcessState.ExitCode() != 5 {
		t.Errorf("the held child reads %v (%v), want its own exit 5: the reap took it", held.ProcessState, err)
	}
}

// The drain's per-process signal (signalEach, while the guard holds a child) passes over a process
// it may not signal, as kill(-1) does, rather than fail and panic PID 1, and over one already gone;
// it signals every other process of the listing, and any other error ends it with that error.
func TestTheDrainsSignalsPassOverAProcessTheyMayNotSignal(t *testing.T) {
	var signalled []int
	kill := func(pid int, _ syscall.Signal) error {
		signalled = append(signalled, pid)
		switch pid {
		case 11:
			return syscall.EPERM
		case 12:
			return syscall.ESRCH
		}
		return nil
	}
	if err := signalEach([]int{10, 11, 12, 13}, syscall.SIGTERM, kill); err != nil {
		t.Fatalf("signalEach answered %v, want nil: a process it may not signal, or one gone, is passed over", err)
	}
	if want := []int{10, 11, 12, 13}; !slices.Equal(signalled, want) {
		t.Errorf("signalEach signalled %v, want every process of the listing, %v", signalled, want)
	}
	signalled = nil
	failing := func(pid int, _ syscall.Signal) error {
		signalled = append(signalled, pid)
		if pid == 11 {
			return syscall.EINVAL
		}
		return nil
	}
	if err := signalEach([]int{10, 11, 12}, syscall.SIGTERM, failing); !errors.Is(err, syscall.EINVAL) || !slices.Equal(signalled, []int{10, 11}) {
		t.Errorf("signalEach answered %v after signalling %v, want EINVAL from process 11 and no signal after it", err, signalled)
	}
}

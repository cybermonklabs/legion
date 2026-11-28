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
	t.Parallel()
	t.Logf("%s", runAsPIDOne(t, "TestControllerDrainRunningHeldPIDOneHelper"))
}

// TestControllerDrainRunningHeldPIDOneHelper is TestTheControllerPodsDrainLeavesARunningHeldChildToFinish's
// body, run as PID 1.
func TestControllerDrainRunningHeldPIDOneHelper(t *testing.T) {
	// Not parallel: a helper child, run in a process of its own; here it returns at once.
	if os.Getenv(pidOneHelperEnv) != "1" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatal("the drain helper is not PID 1")
	}
	// Each held child's exit depends on its own child, as the install's does (its --help run is
	// bash running python): a drain that ends that child makes the shell exit 9, not 5. The child
	// waits for release after the drain, so the drain must encounter it even on a loaded machine.
	const waiter = `sh -c 'while [ ! -e "$1" ]; do sleep 0.01; done' sh "$1"`
	for _, tc := range []struct{ name, script string }{
		// SIGTERM at its default: a SIGTERM the drain sends every process reaches it.
		{"a held child", waiter + " && exit 5; exit 9"},
		// SIGTERM ignored, by its child too, which the guard does not hold: a drain that counts
		// the child as left kills every process once the stop grace is over.
		{"a held child that ignores SIGTERM", "trap '' TERM; " + waiter + " && exit 5; exit 9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := &pidGuard{}
			m := &manager{cfg: Config{StopGrace: time.Second}, guard: guard}
			released := filepath.Join(t.TempDir(), "released")
			// The context is a hang guard alone.
			ctx, cancel := context.WithTimeout(context.Background(), beforeDeadline(t))
			defer cancel()
			held := exec.CommandContext(ctx, "/bin/sh", "-c", tc.script, "sh", released)
			pid, err := guard.start(held)
			if err != nil {
				t.Fatal(err)
			}
			// Wait for the held child's own child before testing the drain; setup is not timed.
			children := fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)
			testwait.WithinPolling(t, beforeDeadline(t), testwait.Poll, "the held child's own child to start", func() bool {
				body, err := os.ReadFile(children)
				return err == nil && strings.TrimSpace(string(body)) != ""
			})
			began := time.Now()
			m.cleanExitedGeneration()
			t.Logf("the drain returned after %s", time.Since(began).Round(time.Millisecond))
			if err := os.WriteFile(released, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			err = held.Wait()
			guard.done(pid)
			if held.ProcessState == nil || held.ProcessState.ExitCode() != 5 {
				t.Errorf("the held child, still running when the drain began, reads %v (%v), want its own exit 5: the drain ended it or its child", held.ProcessState, err)
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
		started, released, finished := filepath.Join(marks, "started"), filepath.Join(marks, "released"), filepath.Join(marks, "finished")
		// Wait for PID 1 to adopt the member so only the group clause can spare it. The member
		// finishes on release after the drain; the held child exits 5 when it finishes, or 9 if
		// the drain ends the member. The context bounds a hang, not the drain's behavior.
		ctx, cancel := context.WithTimeout(context.Background(), beforeDeadline(t))
		defer cancel()
		held := exec.CommandContext(ctx, "/bin/sh", "-c", `( sh -c 'echo $$ >"$1"; while [ ! -e "$3" ]; do sleep 0.01; done; : >"$2"' sh "$1" "$2" "$3" & )
until [ -s "$1" ]; do sleep 0.01; done
member=$(cat "$1")
while [ ! -e "$2" ]; do kill -0 "$member" 2>/dev/null || exit 9; sleep 0.01; done
exit 5`, "sh", started, finished, released)
		procgroup.Configure(held)
		pid, err := guard.start(held)
		if err != nil {
			t.Fatal(err)
		}
		testwait.WithinPolling(t, beforeDeadline(t), testwait.Poll, "PID 1 to adopt the held child's group member", func() bool {
			body, err := os.ReadFile(started)
			if err != nil {
				return false
			}
			member, err := strconv.Atoi(strings.TrimSpace(string(body)))
			ppid, _, ok := statParentAndGroup("/proc", member)
			return err == nil && ok && ppid == 1
		})
		m.cleanExitedGeneration()
		if err := os.WriteFile(released, nil, 0o600); err != nil {
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
	// drain's SIGTERM, is killed at the end of the grace and is reaped, while the held child beside
	// it runs to its own exit. The held child exits once the case releases it, after the drain, so
	// it runs beside the drain however loaded the machine is (it once lived by `sleep 2`, which a
	// slow machine let end before the drain began). The left process sleeps an hour, so the drain
	// returns before that only by killing it, and it must kill it at the grace configured: the
	// drain returns no sooner than the grace and no later than the grace and ten seconds, so a kill
	// at the launcher's default grace of 30 s fails; on a machine with every CPU busy the drain
	// returns about half a second after it began. The wait for the return runs under
	// the test's hang guard, for a drain that never returns, and the bounds are read after it; the
	// left process's reap is waited for under the guard too.
	t.Run("what the generation left is ended around a held child", func(t *testing.T) {
		grace := 500 * time.Millisecond
		guard := &pidGuard{}
		m := &manager{cfg: Config{StopGrace: grace}, guard: guard}
		released := filepath.Join(t.TempDir(), "released")
		ctx, cancel := context.WithTimeout(context.Background(), beforeDeadline(t))
		defer cancel()
		held := exec.CommandContext(ctx, "/bin/sh", "-c", `while [ ! -e "$1" ]; do sleep 0.01; done; exit 5`, "sh", released)
		pid, err := guard.start(held)
		if err != nil {
			t.Fatal(err)
		}
		ignoring := filepath.Join(t.TempDir(), "ignoring")
		left := exec.Command("/usr/bin/setsid", "/bin/sh", "-c", `trap '' TERM; : >"$1"; exec sleep 3600`, "sh", ignoring)
		if err := left.Start(); err != nil {
			t.Fatal(err)
		}
		testwait.WithinPolling(t, beforeDeadline(t), testwait.Poll, "the left process to ignore SIGTERM", func() bool {
			_, err := os.Stat(ignoring)
			return err == nil
		})
		drained := make(chan time.Duration, 1)
		began := time.Now()
		go func() {
			m.cleanExitedGeneration()
			drained <- time.Since(began)
		}()
		var took time.Duration
		select {
		case took = <-drained:
		case <-time.After(beforeDeadline(t)):
			t.Fatal("the drain had not returned under the test's hang guard: it did not kill the process the generation left, which ignores SIGTERM and sleeps an hour")
		}
		t.Logf("the drain returned after %s", took.Round(time.Millisecond))
		if took < grace {
			t.Errorf("the drain returned after %s, want past its %s grace: the process it left ignores SIGTERM", took, grace)
		}
		if took > grace+10*time.Second {
			t.Errorf("the drain returned after %s, past its %s grace and ten seconds: it did not kill the process it left at the grace the case configured (the launcher's default is 30 s)", took, grace)
		}
		// The drain kills the process it left and reaps it before it returns, so the process is
		// gone now, and one read of it says so; a process still there is killed, so it outlives
		// nothing, and named.
		if err := syscall.Kill(left.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
			_ = left.Process.Kill()
			t.Errorf("the process the generation left, %d, was still there after the drain returned (%v): the drain did not end and reap it", left.Process.Pid, err)
		}
		if err := left.Wait(); !reapedByAnother(err) {
			t.Errorf("the process the generation left answered its own wait with %v, want ECHILD or ESRCH: the drain did not end and reap it", err)
		}
		if err := os.WriteFile(released, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err = held.Wait()
		guard.done(pid)
		if held.ProcessState == nil || held.ProcessState.ExitCode() != 5 {
			t.Errorf("the held child reads %v (%v), want its own exit 5: the drain ended it", held.ProcessState, err)
		}
	})
}

// Both of the guard's passes (pidGuard.reap, the drain's, and pidGuard.reapOrphans, the orphan
// reaper's) leave a held child to its own wait and reap an exited process PID 1 adopted from the
// held child's process group, since no other wait will read it: a pass that skipped that orphan
// too would leave it a zombie until some unrelated child's exit. The held child has exited before
// the pass, so a pass that reaped it would take the exit its own wait reads. The two passes reap
// the same set, and only the drain's classifies what is left, so the orphan reaper's reads no
// process's stat. It runs as PID 1, which adopts the orphan
// (TestGuardReapsAHeldGroupsOrphanPIDOneHelper).
func TestTheGuardsReapTakesAnOrphanOfAHeldChildsGroup(t *testing.T) {
	t.Parallel()
	t.Logf("%s", runAsPIDOne(t, "TestGuardReapsAHeldGroupsOrphanPIDOneHelper"))
}

// TestGuardReapsAHeldGroupsOrphanPIDOneHelper is TestTheGuardsReapTakesAnOrphanOfAHeldChildsGroup's
// body, run as PID 1, once for each of the guard's passes.
func TestGuardReapsAHeldGroupsOrphanPIDOneHelper(t *testing.T) {
	// Not parallel: a helper child, run in a process of its own; here it returns at once.
	if os.Getenv(pidOneHelperEnv) != "1" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatal("the reap helper is not PID 1")
	}
	for _, tc := range []struct {
		name string
		pass func(guard *pidGuard) error
	}{
		{"the drain's pass", func(guard *pidGuard) error { _, err := guard.reap(); return err }},
		{"the orphan reaper's pass", func(guard *pidGuard) error { return guard.reapOrphans() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := &pidGuard{}
			marks := t.TempDir()
			orphanFile, released := filepath.Join(marks, "orphan"), filepath.Join(marks, "released")
			// The subshell starts the orphan in the background, writes its pid and exits, so the
			// orphan is adopted by PID 1 in the held child's process group; the held child then
			// exits too, and its exit waits for its own wait. The orphan ends only once the case
			// releases it, after PID 1 has adopted it: a shell reaps a background child that ends
			// while the shell still runs, so an orphan that ended before its subshell did, as a
			// short sleep does when a loaded machine is slow to run the subshell's last line, was
			// the subshell's to reap, and no zombie of PID 1's was ever there to find.
			held := exec.CommandContext(context.Background(), "/bin/sh", "-c",
				`(sh -c 'while [ ! -e "$1" ]; do sleep 0.01; done' sh "$2" & echo $! >"$1"); exit 5`, "sh", orphanFile, released)
			procgroup.Configure(held)
			pid, err := guard.start(held)
			if err != nil {
				t.Fatal(err)
			}
			var orphan int
			// Both exits must be in place before the pass; setup is not timed.
			testwait.WithinPolling(t, beforeDeadline(t), testwait.Poll, "the held child to exit and PID 1 to adopt its orphan", func() bool {
				body, err := os.ReadFile(orphanFile)
				if err != nil {
					return false
				}
				orphan, err = strconv.Atoi(strings.TrimSpace(string(body)))
				ppid, _, ok := statParentAndGroup("/proc", orphan)
				return err == nil && ok && ppid == 1 && zombie(pid)
			})
			if err := os.WriteFile(released, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			testwait.WithinPolling(t, beforeDeadline(t), testwait.Poll, "the orphan PID 1 adopted to exit", func() bool { return zombie(orphan) })
			if err := tc.pass(guard); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", orphan)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the held child's orphan %d is still there after the pass (%v): it stays a zombie until some unrelated child exits", orphan, err)
			}
			err = held.Wait()
			guard.done(pid)
			if held.ProcessState == nil || held.ProcessState.ExitCode() != 5 {
				t.Errorf("the held child reads %v (%v), want its own exit 5: the pass took it", held.ProcessState, err)
			}
		})
	}
}

// The drain's per-process signal (signalEach, while the guard holds a child) passes over a process
// it may not signal, as kill(-1) does, rather than fail and panic PID 1, and over one already gone;
// it signals every other process of the listing, and any other error ends it with that error.
func TestTheDrainsSignalsPassOverAProcessTheyMayNotSignal(t *testing.T) {
	t.Parallel()
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

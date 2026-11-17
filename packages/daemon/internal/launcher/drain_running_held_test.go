package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	for _, tc := range []struct{ name, script string }{
		// SIGTERM at its default: a SIGTERM the drain sends every process reaches it.
		{"a held child", "sleep 2; exit 5"},
		// SIGTERM ignored, by its sleep too, which the guard does not hold: a drain that counts the
		// sleep as left kills every process once the stop grace is over.
		{"a held child that ignores SIGTERM", "trap '' TERM; sleep 2; exit 5"},
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
			testwait.WithinPolling(t, 5*time.Second, testwait.Poll, "the held child's sleep to start", func() bool {
				body, err := os.ReadFile(children)
				return err == nil && strings.TrimSpace(string(body)) != ""
			})
			began := time.Now()
			m.cleanExitedGeneration()
			t.Logf("the drain returned after %s", time.Since(began).Round(time.Millisecond))
			err = held.Wait()
			guard.done(pid)
			if held.ProcessState == nil || held.ProcessState.ExitCode() != 5 {
				t.Errorf("the held child, still running when the drain began, reads %v (%v), want its own exit 5: the drain ended it", held.ProcessState, err)
			}
		})
	}
	// While a child is held the drain signals process by process rather than with kill(-1), and it
	// still ends what the generation left: a setsid'd process that ignores SIGTERM outlives the
	// drain's SIGTERM, is killed at the end of the grace and is reaped, while the held child beside
	// it runs to its own exit.
	t.Run("what the generation left is ended around a held child", func(t *testing.T) {
		grace := 500 * time.Millisecond
		guard := &pidGuard{}
		m := &manager{cfg: Config{StopGrace: grace}, guard: guard}
		held := exec.Command("/bin/sh", "-c", "sleep 2; exit 5")
		pid, err := guard.start(held)
		if err != nil {
			t.Fatal(err)
		}
		ignoring := filepath.Join(t.TempDir(), "ignoring")
		left := exec.Command("/usr/bin/setsid", "/bin/sh", "-c", `trap '' TERM; : >"$1"; exec sleep 30`, "sh", ignoring)
		if err := left.Start(); err != nil {
			t.Fatal(err)
		}
		testwait.WithinPolling(t, 5*time.Second, testwait.Poll, "the left process to ignore SIGTERM", func() bool {
			_, err := os.Stat(ignoring)
			return err == nil
		})
		began := time.Now()
		m.cleanExitedGeneration()
		took := time.Since(began)
		t.Logf("the drain returned after %s", took.Round(time.Millisecond))
		// The left process sleeps 30s, so a drain that never signals it waits for that and fails
		// here too.
		if took < grace || took > grace+10*time.Second {
			t.Errorf("the drain returned after %s, want past its %s grace (the process it left ignores SIGTERM) and well before that process's 30s sleep ends: the drain did not kill it", took, grace)
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
	testwait.WithinPolling(t, 5*time.Second, testwait.Poll, "the held child's orphan to exit", func() bool {
		body, err := os.ReadFile(orphanFile)
		if err != nil {
			return false
		}
		orphan, err = strconv.Atoi(strings.TrimSpace(string(body)))
		return err == nil && zombie(orphan)
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

package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
}

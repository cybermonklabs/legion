//go:build e2e

// The rig's own check that a role's processes end with it, as they end with the role's PID
// namespace in an issue pod (internal/launcher's role-init: a stop's SIGKILL at the end of its
// grace, and Kill(-1, SIGKILL) at the generation's end), which check w5 of this change relies on
// for the seventh claim's named service. Oh My Pi runs a named bash service through its
// worker-daemon broker, a process of its own session that a process-group kill does not reach; in
// a pod the broker and its services are the role's namespace's, and end with it. Each pane of the
// rig keeps a state home of its own, as roleEnvironment gives each role one, and the rig ends every
// process left under that state home once the pane's process group has ended, on a stop and on a
// death, before the daemon can start the claim's next process.
package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// TestW5RigEndsARolesProcessesWithIt runs one claim on the rig whose turn starts a named bash
// service, stops its process as a relaunch stops it, and reads the service gone; then has the
// resumed session start another, kills the process group as a death ends it, and reads that
// service gone before the claim's next process starts.
func TestW5RigEndsARolesProcessesWithIt(t *testing.T) {
	suffix := randomSuffix(t)[:6]
	first, second := "W5-TEAR-1-"+suffix, "W5-TEAR-2-"+suffix
	w := newW5Rig(t, w5Options{script: "scripts/e2e/unreachable-claims.sh", claims: []w5Claim{
		{name: "worker", key: "T1", tree: "T1", phase: phase.Implementing, role: claim.RoleImplementer, provider: "w5tear", task: first + ": start the rig's service."},
	}})
	worker := w.claims["worker"]
	files := t.TempDir()
	firstPid, secondPid := filepath.Join(files, "first.pid"), filepath.Join(files, "second.pid")
	worker.stand.script(promptCarrying(first), 0, "bash", serviceCall("tear-first", firstPid))
	worker.stand.script(promptCarrying(second), 0, "bash", serviceCall("tear-second", secondPid))

	var stoppedLaunch *rigLaunch
	w.step("a-stop-ends-the-role's-service", func() {
		w.boot()
		service := awaitServicePid(w, firstPid)
		launch := w.runtime.current(worker.token)
		stoppedLaunch = launch
		w.record("teardown: the first service runs as pid %d (parent %d), started by %s generation %d",
			service, parentPid(service), worker.token, launch.spec.Generation)
		stopped := time.Now()
		if err := w.runtime.Suspend(context.Background(), launch.pane.locator); err != nil {
			w.fails("stop", "Suspend %s: %v", worker.token, err)
		}
		gone := awaitProcessGone(service, 15*time.Second)
		w.check("rig-stop-ends-the-service", gone, "the service (pid %d) ended within 15 s of its role's stop at %s: %t", service, clockAt(stopped), gone)
	})

	w.step("a-death-ends-the-role's-service-before-the-next-launch", func() {
		w.await("the daemon's relaunch of the stopped worker", w5PaneReady, func() bool {
			current := w.runtime.current(worker.token)
			return current != nil && current != stoppedLaunch && current.running() &&
				w.snapshot().views[worker.token].state == string(supervise.StateIdle)
		})
		if status, body := w.deliver(worker.token, second+": start the rig's second service."); status != http.StatusOK {
			w.fails("deliver", "the operator's task = %d %s", status, body)
		}
		service := awaitServicePid(w, secondPid)
		launch := w.runtime.current(worker.token)
		launches := len(w.runtime.launchesOf(worker.token))
		died := time.Now()
		if err := killGroup(launch.pane.pid); err != nil {
			w.fails("death", "kill %s's process group: %v", worker.token, err)
		}
		gone := awaitProcessGone(service, 15*time.Second)
		goneAt := time.Now()
		var next time.Time
		w.await("the daemon's launch after the death", w5PaneReady, func() bool {
			all := w.runtime.launchesOf(worker.token)
			if len(all) > launches {
				next = all[len(all)-1].at
				return true
			}
			return false
		})
		w.check("rig-death-ends-the-service-before-the-next-launch", gone && goneAt.Before(next.Add(time.Second)),
			"the service (pid %d) ended within 15 s of its role's death at %s: %t (at %s); the next launch started at %s",
			service, clockAt(died), gone, clockAt(goneAt), clockAt(next))
	})
}

// awaitServicePid waits for a service to write its pid, and kills it at the test's end, by that
// pid, if it still runs.
func awaitServicePid(w *w5Rig, path string) int {
	var pid int
	w.await("the service's pid in "+path, w5PaneReady, func() bool {
		pid = readPid(path)
		return pid > 0 && processState(pid) != ""
	})
	broker := parentPid(pid)
	w.t.Cleanup(func() {
		if processState(pid) != "" {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if broker > 1 && isOmpBroker(broker) {
			_ = syscall.Kill(broker, syscall.SIGKILL)
		}
	})
	return pid
}

// awaitProcessGone is whether pid has ended, or is a zombie, within within.
func awaitProcessGone(pid int, within time.Duration) bool {
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if state := processState(pid); state == "" || state == "Z" || state == "X" {
			return true
		}
	}
	return false
}

// parentPid is pid's parent, 0 when pid is gone.
func parentPid(pid int) int {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	text := string(raw)
	end := strings.LastIndexByte(text, ')')
	fields := strings.Fields(text[end+1:])
	if end < 0 || len(fields) < 2 {
		return 0
	}
	parent, _ := strconv.Atoi(fields[1])
	return parent
}

// isOmpBroker is whether pid runs Oh My Pi's worker-daemon broker.
func isOmpBroker(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return err == nil && strings.Contains(string(raw), "__omp_worker_daemon_broker")
}

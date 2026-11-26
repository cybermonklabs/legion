package shim_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// reapHelperEnv carries the plan of TestReapingShimHelper, the test binary re-executed to run one
// shim.Run in a process of its own: a shim with Config.ReapOrphans makes its process a subreaper
// and reaps every exited child it does not wait for itself, which in the test binary would take
// the children of the package's other tests. It is no LEGION_ variable, which fixtureenv's Guard
// would take out of the helper before it reads it.
const reapHelperEnv = "SHIM_REAPER_TEST_HELPER"

// reapHelperPlan is the helper's shim: the stub daemon's socket, the fake Oh My Pi's environment,
// and whether the shim reaps orphans.
type reapHelperPlan struct {
	Address string
	Env     []string
	Reap    bool
}

// TestReapingShimHelper is the helper: it runs the plan's shim to its end and prints the status
// shim.Run returned.
func TestReapingShimHelper(t *testing.T) {
	encoded := os.Getenv(reapHelperEnv)
	if encoded == "" {
		return
	}
	var plan reapHelperPlan
	if err := json.Unmarshal([]byte(encoded), &plan); err != nil {
		fmt.Printf("reap-helper error=%v\n", err)
		os.Exit(0)
	}
	code, err := shim.Run(context.Background(), shim.Config{
		Network: "unix", Address: plan.Address, BootToken: bootToken, Argv: []string{os.Args[0]},
		Env: plan.Env, Grace: grace, ReapOrphans: plan.Reap, Log: os.Stdout,
	})
	fmt.Printf("reap-helper code=%d error=%v\n", code, err)
	os.Exit(0)
}

// A shim told to reap orphans (Config.ReapOrphans, which the controller pod's single-role
// launcher passes) takes in the orphan its agent's tool call leaves (a sleep a shell started in the
// background and left), reaps it once it ends while the agent still runs, and reports the agent's
// own end exactly as before: its exit status, or 128 plus the signal that killed it. Without the
// flag, the control, the orphan goes to another process, as it did before the flag existed, and
// the status is the same. Each shim runs in a process of its own (TestReapingShimHelper), since a
// subreaper takes in the orphans of everything under it.
func TestAReapingShimTakesInItsAgentsOrphansAndKeepsTheAgentsStatus(t *testing.T) {
	for _, ending := range []struct {
		name, frame string
		want        int
	}{
		{"an exit", `{"type":"fake_exit","message":"3"}`, 3},
		{"a signal's death", `{"type":"fake_die"}`, 128 + int(syscall.SIGKILL)},
	} {
		for _, reap := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, reaping orphans %t", ending.name, reap), func(t *testing.T) {
				path := socketPath(t)
				daemon := listen(t, path)
				child := newOMP(t)
				helper := startReapHelper(t, reapHelperPlan{Address: path, Env: child.env, Reap: reap})
				p := daemon.accept(t)
				p.open(t)

				p.sendLine(t, `{"type":"fake_orphan","message":"3"}`)
				orphan := readyPID(t, p.expectRaw(t, "fake_orphan"))
				parent, err := parentOf(orphan)
				if err != nil {
					t.Fatalf("read the orphan %d: %v", orphan, err)
				}
				switch {
				case reap && parent != helper.cmd.Process.Pid:
					t.Errorf("the orphan's parent is %d, want the shim %d", parent, helper.cmd.Process.Pid)
				case !reap && parent == helper.cmd.Process.Pid:
					t.Errorf("without ReapOrphans the orphan's parent is still the shim %d, so the case above shows nothing of the flag's", parent)
				}
				if reap {
					awaitOrphanReaped(t, orphan)
				}

				p.sendLine(t, ending.frame)
				if code := helper.wait(t); code != ending.want {
					t.Errorf("the shim reported %d, want the agent's own %d", code, ending.want)
				}
			})
		}
	}
}

// reapHelper is one running TestReapingShimHelper: its process, its output, and its wait, which a
// goroutine of its own makes from the start, so the test and its cleanup only ever read its end.
type reapHelper struct {
	cmd    *exec.Cmd
	out    *syncBuffer
	done   chan struct{}
	waited error
}

// startReapHelper starts TestReapingShimHelper with plan.
func startReapHelper(t *testing.T, plan reapHelperPlan) *reapHelper {
	t.Helper()
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	h := &reapHelper{cmd: exec.Command(os.Args[0], "-test.run=^TestReapingShimHelper$"), out: newSyncBuffer(), done: make(chan struct{})}
	h.cmd.Env = append(os.Environ(), reapHelperEnv+"="+string(encoded))
	h.cmd.Stdout, h.cmd.Stderr = h.out, h.out
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		h.waited = h.cmd.Wait()
		close(h.done)
	}()
	t.Cleanup(func() {
		select {
		case <-h.done:
		default:
			_ = h.cmd.Process.Kill()
			<-h.done
		}
	})
	return h
}

// wait waits for the helper and answers the status its shim.Run returned. Its one bound is the
// test binary's deadline (testwait.Receive), as the package's other waits since #5417.
func (h *reapHelper) wait(t *testing.T) int {
	t.Helper()
	testwait.Receive(t, "the helper's shim to end", h.done)
	if h.waited != nil {
		t.Fatalf("the helper: %v\n%s", h.waited, h.out)
	}
	_, rest, ok := strings.Cut(h.out.String(), "reap-helper code=")
	if !ok {
		t.Fatalf("the helper reported no status\n%s", h.out)
	}
	spelled, failure, _ := strings.Cut(rest, " error=")
	if !strings.HasPrefix(failure, "<nil>") {
		t.Fatalf("shim.Run failed: %s\n%s", failure, h.out)
	}
	code, err := strconv.Atoi(spelled)
	if err != nil {
		t.Fatalf("the helper reported %q, not a status\n%s", spelled, h.out)
	}
	return code
}

// parentOf reads pid's parent from /proc.
func parentOf(pid int) (int, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	if len(fields) < 2 {
		return 0, fmt.Errorf("an unreadable stat %q", stat)
	}
	return strconv.Atoi(fields[1])
}

// awaitOrphanReaped waits for pid to leave /proc, which it does once the shim, its parent, has
// reaped it while the agent still runs. A wait that gives up names what /proc read of it last: a
// zombie is an orphan that exited and that no one reaped.
func awaitOrphanReaped(t *testing.T, pid int) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the orphan %d to be reaped by the shim", pid), func() (string, bool) {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		switch {
		case errors.Is(err, os.ErrNotExist):
			return "reaped", true
		case err != nil:
			return err.Error(), false
		}
		if fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:])); len(fields) > 0 && fields[0] == "Z" {
			return "a zombie: exited, and not reaped", false
		}
		return "running", false
	})
}

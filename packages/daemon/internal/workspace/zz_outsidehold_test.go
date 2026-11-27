package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// CONTROL (never merged): the hold this control asks for is the machine's own and no process of
// the runner's: a shell started beside this test binary, before it, runs the busy loops and owns
// them, so none of them is a child of this process and no CPU of theirs reaches its rusage. The
// shell watches holdRequest; it starts 16 busy loops pinned to CPU 0, writes holdOn, runs them for
// 1.5 s and then kills them.
const (
	holdRequest = "/work/tmp/outside-hold-request"
	holdOn      = "/work/tmp/outside-hold-on"
)

// CONTROL: heldOnCPU0 is the argv prefix that runs a command on CPU 0 at nice 19, where the
// outside loops keep it waiting for a CPU.
var heldOnCPU0 = []string{"nice", "-n", "19", "taskset", "-c", "0"}

// CONTROL: held is argv under that prefix.
func held(argv ...string) []string {
	return append(append([]string{}, heldOnCPU0...), argv...)
}

// CONTROL: heldTool writes an executable at dir/name that runs path, with its arguments, under
// that prefix in its own process (exec), and answers its path: a runner that is handed a tool
// path, rather than an argv, holds its command through this.
func heldTool(t *testing.T, dir, name, path string) string {
	t.Helper()
	wrapper := filepath.Join(dir, name)
	script := "#!/bin/sh\nexec " + strings.Join(heldOnCPU0, " ") + " '" + path + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatalf("write the held %s: %v", name, err)
	}
	return wrapper
}

// CONTROL: holdOutside asks the shell beside this test for its hold and waits until the loops are
// running, so the command the row starts next meets a CPU 0 that is already busy.
func holdOutside(t *testing.T) {
	t.Helper()
	_ = os.Remove(holdOn)
	if err := os.WriteFile(holdRequest, nil, 0o600); err != nil {
		t.Fatalf("ask the shell beside this test for its hold: %v", err)
	}
	if !testwait.Holds(10*time.Second, testwait.Poll, func() bool { _, err := os.Stat(holdOn); return err == nil }) {
		t.Fatalf("the shell beside this test did not start its busy loops within 10s: no %s", holdOn)
	}
	t.Logf("CONTROL: the hold outside this process is running (16 busy loops on CPU 0, 1.5 s), and the command runs at nice 19 on CPU 0")
}

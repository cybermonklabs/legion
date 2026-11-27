package workspace

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// PLANT (never merged): plantShape is the fault this runner carries, from LEGION_PLANT:
//
//   - "hogs": before it starts a command, the runner starts 16 busy loops of its own on CPU 0 and
//     pins the command there too (taskset), at the runner's own nice. It kills and reaps the loops
//     1.5 s later. The runner's own work keeps its own command off the CPU.
//   - "nice": "hogs", and the command runs at nice 19 as well, so the loops hold it off CPU 0 for
//     the whole 1.5 s.
//   - "nicedetached": "nice", with the loops started through a shell that leaves them behind
//     (setsid, their parent gone), so no wait of this process ever reaps them.
//   - "timeout1s": the runner keeps a second of every Command.Timeout back for itself: the command
//     is bounded by Timeout less a second.
//
// Anything else, the empty value included, leaves the runner as it is.
func plantShape() string { return os.Getenv("LEGION_PLANT") }

// plantHoldCPU is how long the plant's busy loops run.
const plantHoldCPU = 1500 * time.Millisecond

// plantHogs is how many busy loops the plant starts.
const plantHogs = 16

// plantPrefix is argv under the plant's prefix, and the plant's hold started where it has one. A
// shape with no prefix answers argv unchanged.
func plantPrefix(argv []string) []string {
	switch plantShape() {
	case "hogs":
		plantHold(false)
		return append([]string{"taskset", "-c", "0"}, argv...)
	case "nice":
		plantHold(false)
		return append([]string{"nice", "-n", "19", "taskset", "-c", "0"}, argv...)
	case "nicedetached":
		plantHold(true)
		return append([]string{"nice", "-n", "19", "taskset", "-c", "0"}, argv...)
	}
	return argv
}

// plantBound is the Timeout the plant's runner really gives a command.
func plantBound(timeout time.Duration) time.Duration {
	if plantShape() == "timeout1s" {
		return max(time.Nanosecond, timeout-time.Second)
	}
	return timeout
}

// plantHold starts the busy loops and ends them plantHoldCPU later. Detached, they are started
// through a shell that exits at once, so this process waits for none of them and their CPU
// reaches no wait of its own; the shell they leave behind ends them.
func plantHold(detached bool) {
	if detached {
		script := `for i in $(seq 1 ` + itoa(plantHogs) + `); do taskset -c 0 sh -c 'while :; do :; done' & done; ` +
			`sleep ` + plantHoldCPU.String() + `; kill $(jobs -p) 2>/dev/null; wait`
		cmd := exec.Command("setsid", "sh", "-c", script)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err == nil {
			go func() { _, _ = cmd.Process.Wait() }() // the shell exits at once; its loops outlive it.
		}
		return
	}
	var hogs []*exec.Cmd
	for range plantHogs {
		cmd := exec.Command("taskset", "-c", "0", "sh", "-c", "while :; do :; done")
		if err := cmd.Start(); err != nil {
			continue
		}
		hogs = append(hogs, cmd)
	}
	var once sync.Once
	time.AfterFunc(plantHoldCPU, func() {
		once.Do(func() {
			for _, cmd := range hogs {
				_ = cmd.Process.Signal(syscall.SIGKILL)
			}
			for _, cmd := range hogs {
				_ = cmd.Wait()
			}
		})
	})
}

// itoa is strconv.Itoa for a small count, kept here so the plant adds no import to this package.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

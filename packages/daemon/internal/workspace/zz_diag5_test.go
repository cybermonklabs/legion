package workspace

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// DIAG (never merged): psiTotals reads this container's cgroup pressure totals, in µs: io some,
// io full, memory some, memory full (/sys/fs/cgroup/{io,memory}.pressure). A total stands for
// every task of the cgroup, the test process's own threads among them, and not for one task.
func psiTotals() [4]int64 {
	var out [4]int64
	for i, name := range []string{"io", "memory"} {
		raw, err := os.ReadFile("/sys/fs/cgroup/" + name + ".pressure")
		if err != nil {
			out[2*i], out[2*i+1] = -1, -1
			continue
		}
		for j, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if j > 1 {
				break
			}
			_, total, found := strings.Cut(line, "total=")
			if !found {
				continue
			}
			out[2*i+j], _ = strconv.ParseInt(strings.TrimSpace(total), 10, 64)
		}
	}
	return out
}

// DIAG: kernelBlockedFacts says which per-task blocked-time records this kernel keeps.
func kernelBlockedFacts() string {
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "unreadable (" + err.Error() + ")"
		}
		return strings.TrimSpace(string(raw))
	}
	_, schedErr := os.Stat("/proc/self/sched")
	return fmt.Sprintf("kernel.sched_schedstats=%s, kernel.task_delayacct=%s, /proc/self/sched present=%t",
		read("/proc/sys/kernel/sched_schedstats"), read("/proc/sys/kernel/task_delayacct"), schedErr == nil)
}

// DIAG: statFacts reads, of a /proc/<pid>/stat line, majflt (field 12), utime+stime (14 and 15,
// in clock ticks) and delayacct_blkio_ticks (42).
func statFacts(line string) string {
	rp := strings.LastIndexByte(line, ')')
	if rp < 0 || rp+2 >= len(line) {
		return fmt.Sprintf("an unreadable stat line %q", line)
	}
	fields := strings.Fields(line[rp+2:])
	field := func(n int) string {
		if n-3 >= len(fields) {
			return "absent"
		}
		return fields[n-3]
	}
	utime, _ := strconv.ParseInt(field(14), 10, 64)
	stime, _ := strconv.ParseInt(field(15), 10, 64)
	return fmt.Sprintf("majflt %s, cpu %s, delayacct_blkio_ticks %s", field(12), time.Duration(utime+stime)*10*time.Millisecond, field(42))
}

// DIAG: logBlocked logs, for one row, the stand-in's wall time from the kernel's start of it to
// its fact (span less began), its run-queue wait on the way (waited), what its stat line at the
// fact says, and the cgroup's pressure totals over the call (since before).
func logBlocked(t *testing.T, row string, span, began, waited time.Duration, known bool, statLine string, before [4]int64) {
	t.Helper()
	after := psiTotals()
	delay := "none: this kernel keeps no run-queue wait"
	if known {
		delay = waited.String()
	}
	t.Logf("DIAG5 %s: wall from the stand-in's start to its fact %s, run_delay %s; at its fact %s; cgroup pressure over the call: io some %dµs full %dµs, memory some %dµs full %dµs; %s",
		row, span-began, delay, statFacts(strings.TrimSpace(statLine)),
		after[0]-before[0], after[1]-before[1], after[2]-before[2], after[3]-before[3], kernelBlockedFacts())
}

// DIAG: readStat reads the stat line a stand-in recorded at path, or says why there is none.
func readStat(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(no stat line: " + err.Error() + ")"
	}
	return string(raw)
}

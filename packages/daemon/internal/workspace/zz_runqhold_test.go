package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// CONTROL (never merged): heldOnCPU0 is the argv prefix that runs a command on CPU 0 at nice 19,
// in its own process (nice and taskset each exec the next), where holdCPU0For's busy loops keep
// it waiting for a CPU.
var heldOnCPU0 = []string{"nice", "-n", "19", "taskset", "-c", "0"}

// CONTROL: held is argv run under heldOnCPU0.
func held(argv ...string) []string {
	return append(append([]string{}, heldOnCPU0...), argv...)
}

// CONTROL: runqHold is how long the rows' commands are held on CPU 0's run queue: past main's
// second, so main's form of each row has not reached its fact when its budget or Timeout ends, and
// short of the head's 2 s Timeout of execRunner's rows (stopTimeout) by enough for the command
// to reach its fact once the hold is over.
const runqHold = 1500 * time.Millisecond

// CONTROL: heldTool writes an executable at dir/name that runs path, with its arguments, on CPU 0
// at nice 19 in its own process (heldOnCPU0, exec), and answers its path. Write it before
// holdCPU0For starts its loops, so no fork of theirs holds it open for writing when it runs.
func heldTool(t *testing.T, dir, name, path string) string {
	t.Helper()
	wrapper := filepath.Join(dir, name)
	script := "#!/bin/sh\nexec " + strings.Join(heldOnCPU0, " ") + " '" + path + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatalf("write the held %s: %v", name, err)
	}
	return wrapper
}

// CONTROL: holdCPU0For keeps hogs busy loops pinned to CPU 0 for d, the machine's own load and
// no process of the runner's, and then SIGKILLs every loop before it waits for any. Just before
// that it reads, of each process this test process started other than the loops, and of each
// member of that process's group: its wall time since the kernel made it, its CPU time, its
// run-queue wait (schedstat run_delay) and its major faults; the test logs them as it ends. A
// command the loops held for all of d waited for a CPU for about all of it.
func holdCPU0For(t *testing.T, hogs int, d time.Duration) {
	t.Helper()
	var procs []*exec.Cmd
	mine := map[int]bool{}
	for range hogs {
		cmd := exec.Command("taskset", "-c", "0", "sh", "-c", "while :; do :; done")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start a hog: %v", err)
		}
		procs = append(procs, cmd)
		mine[cmd.Process.Pid] = true
	}
	began := time.Now()
	var once sync.Once
	var seen string
	end := func() {
		once.Do(func() {
			seen = fmt.Sprintf("the hold ended %s after it began; %s", time.Since(began), heldProcesses(mine))
			for _, cmd := range procs {
				_ = cmd.Process.Signal(syscall.SIGKILL)
			}
			for _, cmd := range procs {
				_ = cmd.Wait()
			}
		})
	}
	timer := time.AfterFunc(d, end)
	t.Cleanup(func() {
		timer.Stop()
		end()
		t.Logf("CONTROL run-queue hold of %d busy loops on CPU 0 for %s: %s", hogs, d, seen)
	})
}

// heldProcesses describes, from /proc, each process whose parent is this test process and that
// is not in skip, and each member of its process group: pid:comm:state, wall since the kernel
// made it, CPU (utime+stime), run_delay, and majflt.
func heldProcesses(skip map[int]bool) string {
	self := os.Getpid()
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "read /proc/uptime: " + err.Error()
	}
	upField, _, _ := strings.Cut(string(raw), " ")
	up, _ := strconv.ParseFloat(upField, 64)
	entries, _ := os.ReadDir("/proc")
	type stat struct {
		comm                         string
		state                        byte
		ppid, pgrp                   int
		majflt, utime, stime, starts int64
	}
	stats := map[int]stat{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		line, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(line)
		lp, rp := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if lp < 0 || rp < 0 || rp+2 >= len(s) {
			continue
		}
		fields := strings.Fields(s[rp+2:])
		if len(fields) < 20 {
			continue
		}
		// fields[0] is stat's field 3 (state): field n is fields[n-3].
		number := func(n int) int64 { v, _ := strconv.ParseInt(fields[n-3], 10, 64); return v }
		stats[pid] = stat{comm: s[lp+1 : rp], state: fields[0][0], ppid: int(number(4)), pgrp: int(number(5)),
			majflt: number(12), utime: number(14), stime: number(15), starts: number(22)}
	}
	const tick = 100.0 // clock ticks per second (sysconf CLK_TCK on Linux)
	var out []string
	for pid, st := range stats {
		if st.ppid != self || skip[pid] {
			continue
		}
		for member, ms := range stats {
			if ms.pgrp != pid && member != pid {
				continue
			}
			delay := "unread"
			if line, err := os.ReadFile("/proc/" + strconv.Itoa(member) + "/schedstat"); err == nil {
				if f := strings.Fields(string(line)); len(f) >= 2 {
					if ns, err := strconv.ParseInt(f[1], 10, 64); err == nil {
						delay = time.Duration(ns).String()
					}
				}
			}
			wall := time.Duration((up - float64(ms.starts)/tick) * float64(time.Second))
			cpu := time.Duration(float64(ms.utime+ms.stime) / tick * float64(time.Second))
			out = append(out, fmt.Sprintf("%d:%s:%c wall %s, cpu %s, run_delay %s, majflt %d", member, ms.comm, ms.state, wall.Round(time.Millisecond), cpu, delay, ms.majflt))
		}
	}
	if len(out) == 0 {
		return "no process of this test's but the loops"
	}
	return strings.Join(out, "; ")
}

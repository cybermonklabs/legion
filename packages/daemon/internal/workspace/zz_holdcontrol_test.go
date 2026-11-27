package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// DIAG (never merged): bootNow is CLOCK_BOOTTIME, the clock /proc/uptime counts, at ns resolution.
func bootNow() time.Duration {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	return time.Duration(ts.Nano())
}

// DIAG: hogRecord is one busy loop's end: when the control sent its SIGKILL, when the watcher
// first saw it dead (zombie or gone), and when the control's Wait reaped it.
type hogRecord struct {
	pid                int
	kill, died, reaped time.Duration
}

// DIAG: hold is what the control did and when, on CLOCK_BOOTTIME.
type hold struct {
	mu      sync.Mutex
	sawPath time.Duration
	ended   time.Duration
	hogs    []hogRecord
}

// CONTROL (never merged): holdOnCPU0 starts hogs busy loops pinned to CPU 0, the machine's own
// load and no process of the runner's, and ends them once path exists, or at the test's end.
func holdOnCPU0(t *testing.T, hogs int, path string) *hold {
	t.Helper()
	h := &hold{}
	var procs []*exec.Cmd
	for range hogs {
		cmd := exec.Command("taskset", "-c", "0", "sh", "-c", "while :; do :; done")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start a hog: %v", err)
		}
		procs = append(procs, cmd)
		h.hogs = append(h.hogs, hogRecord{pid: cmd.Process.Pid})
	}
	var once sync.Once
	end := func() {
		once.Do(func() {
			for i, cmd := range procs {
				at := bootNow()
				_ = cmd.Process.Signal(syscall.SIGKILL)
				_ = cmd.Wait()
				reaped := bootNow()
				h.mu.Lock()
				h.hogs[i].kill, h.hogs[i].reaped = at, reaped
				h.mu.Unlock()
			}
			h.mu.Lock()
			h.ended = bootNow()
			h.mu.Unlock()
		})
	}
	t.Cleanup(end)
	go func() {
		testwait.Holds(time.Minute, testwait.Poll, func() bool { _, err := os.Stat(path); return err == nil })
		h.mu.Lock()
		h.sawPath = bootNow()
		h.mu.Unlock()
		end()
	}()
	return h
}

// DIAG: procStat reads /proc/<pid>/stat: its state letter and process group, ok false once the
// process is gone (reaped).
func procStat(pid int) (state byte, pgrp int, comm string, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, "", false
	}
	s := string(raw)
	lp, rp := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if lp < 0 || rp < 0 || rp+2 >= len(s) {
		return 0, 0, "", false
	}
	comm = s[lp+1 : rp]
	fields := strings.Fields(s[rp+2:])
	if len(fields) < 3 {
		return 0, 0, "", false
	}
	pgrp, _ = strconv.Atoi(fields[2])
	return fields[0][0], pgrp, comm, true
}

// DIAG: termPending reads whether SIGTERM is pending at pid, shared or its own (/proc/<pid>/status
// ShdPnd, SigPnd: SIGTERM is bit 14).
func termPending(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || (name != "ShdPnd" && name != "SigPnd") {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		if err == nil && mask&(1<<14) != 0 {
			return true
		}
	}
	return false
}

// DIAG: stopWatch is what a watcher of the stand-in's group saw after its ready file appeared,
// every 2ms, on CLOCK_BOOTTIME.
type stopWatch struct {
	readySeen, termSeen, leaderExited, groupGone time.Duration
	pid                                          int
	note                                         string
	members                                      []memberChange
	done                                         chan struct{}
}

// DIAG: memberChange is the stand-in's group as the watcher saw it change: its members as
// pid:comm:state, at a moment on CLOCK_BOOTTIME.
type memberChange struct {
	at  time.Duration
	set string
}

// DIAG: watchStop waits for ready, reads the leader's pid from pidFile, and then watches the
// leader, its group and the control's hogs until the group and every hog are gone.
func watchStop(t *testing.T, ready, pidFile string, h *hold) *stopWatch {
	w := &stopWatch{done: make(chan struct{})}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		defer close(w.done)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
		w.readySeen = bootNow()
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			w.note = "no pid file: " + err.Error()
			return
		}
		w.pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		last := ""
		for deadline := bootNow() + 20*time.Second; bootNow() < deadline; {
			now := bootNow()
			if w.termSeen == 0 && termPending(w.pid) {
				w.termSeen = now
			}
			if w.leaderExited == 0 {
				if state, _, _, ok := procStat(w.pid); !ok || state == 'Z' {
					w.leaderExited = now
				}
			}
			if w.groupGone == 0 && syscall.Kill(-w.pid, 0) == syscall.ESRCH {
				w.groupGone = now
			}
			allDead := true
			h.mu.Lock()
			for i := range h.hogs {
				if h.hogs[i].died != 0 {
					continue
				}
				if state, _, _, ok := procStat(h.hogs[i].pid); !ok || state == 'Z' {
					h.hogs[i].died = now
				} else {
					allDead = false
				}
			}
			h.mu.Unlock()
			if w.groupGone == 0 {
				entries, _ := os.ReadDir("/proc")
				var members []string
				for _, entry := range entries {
					pid, err := strconv.Atoi(entry.Name())
					if err != nil {
						continue
					}
					if state, pgrp, comm, ok := procStat(pid); ok && pgrp == w.pid {
						members = append(members, fmt.Sprintf("%d:%s:%c", pid, comm, state))
					}
				}
				if joined := strings.Join(members, ","); joined != last {
					w.members = append(w.members, memberChange{at: now, set: joined})
					last = joined
				}
			}
			if w.groupGone != 0 && allDead {
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	return w
}

// DIAG: timeline logs every moment the row, the control and the watcher recorded, in ms after
// the budget passed (passedBoot), with the stand-in's run-queue waits (its schedstat run_delay) at
// its ready mark and on either side of its trap's rm.
func timeline(t *testing.T, passedBoot, returnedBoot, reached time.Duration, readyWait string, trapFile string, h *hold, w *stopWatch) {
	t.Helper()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Logf("DIAG the watcher had not finished 5s after the run returned")
	}
	ms := func(at time.Duration) string {
		if at == 0 {
			return "never"
		}
		return fmt.Sprintf("%+.1f", float64(at-passedBoot)/float64(time.Millisecond))
	}
	delay := func(line string) time.Duration {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return -1
		}
		ns, _ := strconv.ParseInt(fields[1], 10, 64)
		return time.Duration(ns)
	}
	uptime := func(line string) time.Duration {
		field, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		seconds, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return 0
		}
		return time.Duration(seconds * float64(time.Second))
	}
	var trapIn, trapOut time.Duration
	var waitIn, waitOut time.Duration = -1, -1
	if raw, err := os.ReadFile(trapFile); err == nil {
		lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		if len(lines) == 4 {
			trapIn, waitIn, trapOut, waitOut = uptime(lines[0]), delay(lines[1]), uptime(lines[2]), delay(lines[3])
		}
	} else {
		t.Logf("DIAG no trap record: %v", err)
	}
	h.mu.Lock()
	var kills []string
	var lastDied, lastReaped time.Duration
	alive := func(at time.Duration) int {
		if at == 0 {
			return -1
		}
		n := 0
		for _, hog := range h.hogs {
			if hog.died == 0 || hog.died > at {
				n++
			}
		}
		return n
	}
	for _, hog := range h.hogs {
		kills = append(kills, ms(hog.kill)+"/"+ms(hog.died)+"/"+ms(hog.reaped))
		lastDied, lastReaped = max(lastDied, hog.died), max(lastReaped, hog.reaped)
	}
	aliveAt := fmt.Sprintf("hogs alive at the budget's pass %d, at SIGTERM pending %d, at the trap's start %d, at the leader's exit %d", alive(passedBoot), alive(w.termSeen), alive(trapIn), alive(w.leaderExited))
	sawPath, ended := h.sawPath, h.ended
	h.mu.Unlock()
	readyDelay := delay(readyWait)
	t.Logf("DIAG timeline, ms after the budget passed: ready mark %s (uptime, 10ms); watcher saw ready %s; hog goroutine saw ready %s; SIGTERM pending at leader %s; trap began %s (uptime); rm done %s (uptime); leader exited %s; group gone %s; run returned %s; last hog died %s; last hog reaped %s; hogs all reaped %s; %s",
		ms(reached), ms(w.readySeen), ms(sawPath), ms(w.termSeen), ms(trapIn), ms(trapOut), ms(w.leaderExited), ms(w.groupGone), ms(returnedBoot), ms(lastDied), ms(lastReaped), ms(ended), aliveAt)
	t.Logf("DIAG stand-in run_delay: at ready mark %s; at trap start %s (+%s since ready); after rm %s (+%s over the trap)", readyDelay, waitIn, waitIn-readyDelay, waitOut, waitOut-waitIn)
	t.Logf("DIAG hogs kill/died/reaped: %s", strings.Join(kills, " "))
	var members []string
	for _, change := range w.members {
		members = append(members, ms(change.at)+"["+change.set+"]")
	}
	t.Logf("DIAG group members, ms after the budget passed [pid:comm:state]: %s%s", strings.Join(members, " "), w.note)
}

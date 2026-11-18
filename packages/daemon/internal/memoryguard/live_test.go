//go:build linux

package memoryguard

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The live test's environment. TestMemoryGuardLiveSandbox runs only where liveEnv is 1, which the
// measurement Jobs of INFRA-3557 set and CI does not. liveHolderEnv is the test's own: it makes the
// copy of the test binary the test starts the holder (holdMemory).
const (
	liveEnv = "LEGION_MEMORY_GUARD_LIVE"
	// liveGuardEnv is `on`, the guard at its defaults or at liveSettingsEnv's, or `off`, no guard
	// at all: the negative control, which the kernel is expected to end.
	liveGuardEnv = "LEGION_MEMORY_GUARD_LIVE_GUARD"
	// liveSettingsEnv is the JSON of a shimwire.LauncherMemoryGuard, or empty for the defaults.
	liveSettingsEnv = "LEGION_MEMORY_GUARD_LIVE_SETTINGS"
	// liveRunEnv is the scenario, run as `/bin/sh -c`. A growth scenario starts with `exec`, so the
	// scenario's root process is the one that grows, and the one the guard must end.
	liveRunEnv = "LEGION_MEMORY_GUARD_LIVE_RUN"
	// liveExpectEnv is `ended` or `kept`, what the guard must do to the scenario; the guard off
	// ignores it.
	liveExpectEnv = "LEGION_MEMORY_GUARD_LIVE_EXPECT"
	// liveSecondsEnv is the span over which the guard must keep a kept scenario: one still alive at
	// its end has been kept. A scenario to be ended is waited for with no span of its own.
	liveSecondsEnv = "LEGION_MEMORY_GUARD_LIVE_SECONDS"
	// liveCPUSecondsEnv is how long the guard runs alone before the scenario, for its CPU time.
	liveCPUSecondsEnv = "LEGION_MEMORY_GUARD_LIVE_CPU_SECONDS"
	liveHolderEnv     = "LEGION_MEMORY_GUARD_LIVE_HOLDER"
)

const (
	liveProc   = "/proc"
	liveCgroup = "/sys/fs/cgroup/memory"
	// liveHolderBytes is what the holder allocates and writes: memory in a process the guard may
	// end, smaller than any scenario it must end, so a guard that ends the holder ended the wrong
	// process.
	liveHolderBytes = 256 << 20
	// liveIdleSleeps is how many idle processes the test starts beside the holder, so that a scan
	// reads about as many processes as an issue pod of six roles holds.
	liveIdleSleeps        = 50
	liveSampleEvery       = 100 * time.Millisecond
	liveSecondsDefault    = 120
	liveCPUSecondsDefault = 60
	// liveRelayLineBytes bounds one relayed line of the holder's or the scenario's output, so each
	// is one write that no other line splits.
	liveRelayLineBytes = 1024
	// liveStamp is RFC 3339 with milliseconds, the time every line carries.
	liveStamp = "2006-01-02T15:04:05.000Z07:00"

	liveDeadlineReason = "a wait or a window was still open 30s before the test binary's deadline (a wait's testwait line on stderr names what it waited for and what it last read): run with a longer -timeout"
	liveGuardOffReason = "the guard turned off for good; its off line says why"
)

// TestMemoryGuardLiveSandbox runs the real guard, with the pidfd kill and the wall clock, on this
// machine's own /proc and memory cgroup, against one scenario (liveRunEnv). INFRA-3557's Jobs run
// it in gVisor pods of each size, and its lines are the record the act share's default is set
// from. The guard has no classifier, so it protects the test process alone, as the launcher
// protects itself.
//
// Every line goes straight to stdout, one write each and none buffered, starts `live: ` and ends
// with its time (`at=`), so a pod the kernel ends keeps every line written before the end. The
// last line is `live: verdict PASS` or `live: verdict FAIL reason=…`, except in a run the kernel
// ends, which is what the guard off is expected to end in.
func TestMemoryGuardLiveSandbox(t *testing.T) {
	if os.Getenv(liveHolderEnv) == "1" {
		holdMemory()
	}
	if os.Getenv(liveEnv) != "1" {
		t.Skip("the live test runs only where " + liveEnv + "=1: it drives the guard on this machine's own memory")
	}
	out := &liveLog{}
	r := &liveRun{t: t, out: out}
	// A wait that reaches the shared wait's bound fails the test from inside run, which then
	// answers nothing: this keeps the verdict line last for that failure too.
	judged := false
	defer func() {
		if !judged && t.Failed() {
			out.printf("verdict FAIL reason=%s", liveDeadlineReason)
		}
	}()
	reason := r.run()
	judged = true
	if reason != "" {
		out.printf("verdict FAIL reason=%s", reason)
		t.Fatal(reason)
	}
	out.printf("verdict PASS")
}

// holdMemory is the holder, the test binary started again with liveHolderEnv: it allocates
// liveHolderBytes, writes every page so each is resident, says so on stdout, and sleeps until it
// is killed.
func holdMemory() {
	held := make([]byte, liveHolderBytes)
	page := os.Getpagesize()
	for i := 0; i < len(held); i += page {
		held[i] = 1
	}
	fmt.Printf("held bytes=%d\n", len(held))
	for {
		time.Sleep(time.Hour)
		runtime.KeepAlive(held)
	}
}

// liveConfig is the run the environment asks for.
type liveConfig struct {
	guardOn bool
	// settings are what the guard runs by; wire is liveSettingsEnv's, nil for the defaults.
	settings Settings
	wire     *shimwire.LauncherMemoryGuard
	run      string
	expect   string
	// bound is the span a kept scenario must be kept over (liveSecondsEnv).
	bound     time.Duration
	cpuWindow time.Duration
}

func readLiveConfig() (liveConfig, error) {
	c := liveConfig{settings: Defaults(), run: os.Getenv(liveRunEnv)}
	switch mode := os.Getenv(liveGuardEnv); mode {
	case "on":
		c.guardOn = true
	case "off":
	default:
		return c, fmt.Errorf("%s is %q, want on or off", liveGuardEnv, mode)
	}
	if strings.TrimSpace(c.run) == "" {
		return c, fmt.Errorf("%s names no scenario", liveRunEnv)
	}
	var err error
	if c.bound, err = liveSeconds(liveSecondsEnv, liveSecondsDefault); err != nil {
		return c, err
	}
	if c.cpuWindow, err = liveSeconds(liveCPUSecondsEnv, liveCPUSecondsDefault); err != nil {
		return c, err
	}
	if !c.guardOn {
		return c, nil
	}
	switch c.expect = os.Getenv(liveExpectEnv); c.expect {
	case "ended", "kept":
	default:
		return c, fmt.Errorf("%s is %q, want ended or kept", liveExpectEnv, c.expect)
	}
	if text := os.Getenv(liveSettingsEnv); text != "" {
		var w shimwire.LauncherMemoryGuard
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&w); err != nil {
			return c, fmt.Errorf("%s: %w", liveSettingsEnv, err)
		}
		if dec.More() {
			return c, fmt.Errorf("%s holds more than one JSON object", liveSettingsEnv)
		}
		if err := w.Validate(); err != nil {
			return c, fmt.Errorf("%s: %w", liveSettingsEnv, err)
		}
		c.wire = &w
		c.settings = SettingsOf(w)
	}
	return c, nil
}

// liveSeconds is the whole number of seconds name holds, or fallback where it holds none.
func liveSeconds(name string, fallback int) (time.Duration, error) {
	text := os.Getenv(name)
	if text == "" {
		return time.Duration(fallback) * time.Second, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s is %q, want a whole number of seconds above 0", name, text)
	}
	return time.Duration(n) * time.Second, nil
}

// liveLog writes the live test's lines to stdout, each whole line in one write and none buffered,
// so a pod the kernel ends keeps every line written before the end. A line starts `live: ` and
// ends with its time.
type liveLog struct{ mu sync.Mutex }

// printf writes one line, stamped with the time it is written.
func (l *liveLog) printf(format string, args ...any) {
	l.write(fmt.Sprintf(format, args...) + " at=" + liveTime(time.Now()))
}

// write writes text, which carries its own time, as one line.
func (l *liveLog) write(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = os.Stdout.WriteString("live: " + text + "\n")
}

func liveTime(t time.Time) string { return t.UTC().Format(liveStamp) }

// liveMS is d in milliseconds, or none for a d below 0, which is a time never measured.
func liveMS(d time.Duration) string {
	if d < 0 {
		return "none"
	}
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64)
}

// liveRun is one run of the live test, its steps in the order run takes them.
type liveRun struct {
	t   *testing.T
	out *liveLog
	cfg liveConfig
	// ctx ends at the shared wait's bound (testwait.Context), and ends the windows the harness
	// spends (measureCPU, awaitKept), which are not waits for a fact.
	ctx context.Context

	holder    *liveChild
	guard     *Guard
	watch     *guardWatch
	stopGuard func()

	scenario    *liveChild
	sampler     *liveSampler
	samplerStop chan struct{}
	samplerDone chan struct{}
	finished    bool
}

// run takes the run's steps and answers why it fails, or "" when it passes. Each step's processes
// are ended before it returns, except the holder's and the idle sleeps', which the test's cleanup
// ends after the verdict line.
func (r *liveRun) run() string {
	cfg, err := readLiveConfig()
	if err != nil {
		return err.Error()
	}
	r.cfg = cfg
	r.ctx = testwait.Context(r.t)
	defer r.teardown()
	if reason := r.printStart(); reason != "" {
		return reason
	}
	if reason := r.startHelpers(); reason != "" {
		return reason
	}
	if cfg.guardOn {
		r.startGuard()
		if reason := r.measureCPU(); reason != "" {
			return reason
		}
	}
	if reason := r.startScenario(); reason != "" {
		return reason
	}
	switch {
	case !cfg.guardOn:
		return r.awaitOff()
	case cfg.expect == "ended":
		return r.awaitEnded()
	default:
		return r.awaitKept()
	}
}

// printStart prints the start line: the guard's mode and settings, the limit and AnonPages read
// once, the mark, the kernel's version (gVisor's own under gVisor), and the test's pid.
func (r *liveRun) printStart() string {
	anon, _, err := liveMeminfo()
	if err != nil {
		return err.Error()
	}
	limit, err := liveCount(filepath.Join(liveCgroup, "memory.limit_in_bytes"))
	switch {
	case err != nil:
		return err.Error()
	case limit == 0 || limit >= noLimit:
		return fmt.Sprintf("the container's limit reads %d: the live test needs a memory limit", limit)
	}
	version, err := os.ReadFile(filepath.Join(liveProc, "version"))
	if err != nil {
		return err.Error()
	}
	mode, settings, mark := "off", "settings=none", "none"
	if r.cfg.guardOn {
		s, from := r.cfg.settings, "defaults"
		if r.cfg.wire != nil {
			from = liveSettingsEnv
		}
		mode = "on"
		settings = fmt.Sprintf("settings=%s act_percent=%d floor_percent=%d wait=%s interval=%s",
			from, s.ActPercent, s.FloorPercent, s.Wait, s.Interval)
		mark = strconv.FormatUint(share(limit, s.ActPercent), 10)
	}
	r.out.printf("start guard=%s %s limit=%d anon_pages=%d mark=%s pid=%d expect=%s seconds=%d cpu_seconds=%d version=%q run=%q",
		mode, settings, limit, anon, mark, os.Getpid(), cmp.Or(r.cfg.expect, "none"),
		int(r.cfg.bound/time.Second), int(r.cfg.cpuWindow/time.Second), strings.TrimSpace(string(version)), r.cfg.run)
	return ""
}

// startHelpers starts the holder, waits until it holds its memory, and starts the idle sleeps.
// The test's cleanup kills and waits for each, whatever the outcome.
func (r *liveRun) startHelpers() string {
	ready := make(chan struct{})
	var once sync.Once
	cmd := exec.Command(os.Args[0], "-test.run=^TestMemoryGuardLiveSandbox$")
	cmd.Env = append(os.Environ(), liveHolderEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	holder, err := startLiveChild(r.out, "holder", cmd, func(line string) {
		if strings.HasPrefix(line, "held ") {
			once.Do(func() { close(ready) })
		}
	})
	if err != nil {
		return fmt.Sprintf("start the holder: %v", err)
	}
	r.holder = holder
	r.t.Cleanup(func() { holder.end(r.t) })
	pid := holder.cmd.Process.Pid
	testwait.Until(r.t, fmt.Sprintf("the holder %d to say it holds its memory, or to end (the reading is its /proc status)", pid), func() (string, bool) {
		select {
		case <-ready:
			return "", true
		case <-holder.done:
			return "", true
		default:
			return liveStatus(pid), false
		}
	})
	select {
	case <-ready:
	default:
		return fmt.Sprintf("the holder ended (%s) before it held its memory", holder.state)
	}
	for range liveIdleSleeps {
		sleep := exec.Command("sleep", "3600")
		sleep.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := sleep.Start(); err != nil {
			return fmt.Sprintf("start an idle sleep: %v", err)
		}
		r.t.Cleanup(func() {
			_ = sleep.Process.Kill()
			_ = sleep.Wait()
		})
	}
	anon, _, err := liveMeminfo()
	if err != nil {
		return err.Error()
	}
	r.out.printf("helpers holder_pid=%d holder_bytes=%d idle_sleeps=%d anon_pages=%d",
		holder.cmd.Process.Pid, liveHolderBytes, liveIdleSleeps, anon)
	return ""
}

// startGuard starts the real guard, its lines printed as the test's own, its readings and kills
// watched (guardWatch), at the settings the run asks for.
func (r *liveRun) startGuard() {
	r.watch = &guardWatch{act: r.cfg.settings.ActPercent, notify: make(chan struct{}, 1)}
	g := New(Config{
		Sink: r.watch.sink,
		Logf: func(format string, args ...any) { r.out.printf("guard: %s", fmt.Sprintf(format, args...)) },
	})
	g.observe = r.watch.observe
	if r.cfg.wire != nil {
		g.Apply(r.cfg.wire)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Run(ctx)
	}()
	r.guard = g
	r.stopGuard = sync.OnceFunc(func() {
		cancel()
		testwait.Receive(r.t, "the guard's loop to return once its context ended", done)
	})
}

// measureCPU runs the guard alone for the CPU window, with no scenario and no sampling, and
// prints the CPU time the test process used over it: the guard's own plus that of the idle rest of
// the test process (the Go runtime, and the goroutines that wait on the holder and relay its
// output), which one process cannot tell apart. The window starts as the guard does, so its first
// reading, and the scan for the hourly line that reading starts, count in it.
//
// The window is a measurement and not a wait for a fact: the CPU time is read over the span the
// run names (liveCPUSecondsEnv), so the harness spends all of it, and no event of the guard's
// ends it. Only ctx, at the shared wait's bound, cuts it short.
func (r *liveRun) measureCPU() string {
	userBefore, sysBefore, err := liveCPU()
	if err != nil {
		return fmt.Sprintf("getrusage: %v", err)
	}
	start := time.Now()
	select {
	case <-time.After(r.cfg.cpuWindow):
	case <-r.ctx.Done():
		return liveDeadlineReason
	}
	userAfter, sysAfter, err := liveCPU()
	if err != nil {
		return fmt.Sprintf("getrusage: %v", err)
	}
	elapsed := time.Since(start)
	user, sys := userAfter-userBefore, sysAfter-sysBefore
	r.out.printf("cpu window=%ds user=%.3f sys=%.3f percent_of_core=%.3f",
		int(r.cfg.cpuWindow/time.Second), user.Seconds(), sys.Seconds(), 100*(user+sys).Seconds()/elapsed.Seconds())
	if r.guard.isDead() {
		return liveGuardOffReason
	}
	return ""
}

// liveCPU is the user and system CPU time the test process has used so far, every thread of it.
func liveCPU() (user, sys time.Duration, err error) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, 0, err
	}
	return time.Duration(usage.Utime.Nano()), time.Duration(usage.Stime.Nano()), nil
}

// startScenario starts the scenario in a process group of its own, so the test can end whatever
// it started, and the sampler beside it.
func (r *liveRun) startScenario() string {
	cmd := exec.Command("/bin/sh", "-c", r.cfg.run)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	scenario, err := startLiveChild(r.out, "scenario", cmd, nil)
	if err != nil {
		return fmt.Sprintf("start the scenario: %v", err)
	}
	r.scenario = scenario
	r.out.printf("scenario pid=%d", cmd.Process.Pid)
	r.sampler = &liveSampler{out: r.out, root: cmd.Process.Pid}
	r.samplerStop, r.samplerDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(r.samplerDone)
		r.sampler.run(scenario.done, r.samplerStop)
	}()
	return ""
}

// awaitOff waits, with no guard, for the scenario to end. The kernel is expected to end the pod
// first, so a scenario that ends by itself is a negative control that never reached the kill, and
// a run the kernel does not end fails at the shared wait's bound.
func (r *liveRun) awaitOff() string {
	testwait.Until(r.t, "the scenario to end, which the kernel is expected to forestall by ending the pod (the reading is the scenario's last sample)", func() (string, bool) {
		return r.sampler.latestLine(), !r.scenario.alive()
	})
	r.finishScenario()
	return fmt.Sprintf("with no guard the scenario ended by itself (%s) before the kernel ended the pod: the negative control never reached the kernel's kill",
		r.scenario.state)
}

// awaitEnded waits for the guard to end the scenario, and passes when its first kill is the
// scenario's root, the root ended by SIGKILL, and the holder is alive. It prints the kill's
// measurements whenever the kill was the root's.
func (r *liveRun) awaitEnded() string {
	root := r.scenario.cmd.Process.Pid
	k, reason := r.awaitFirstKill()
	if reason != "" {
		return reason
	}
	if k.PID != root {
		r.finishScenario()
		return fmt.Sprintf(`the guard ended pid %d and not the scenario's root %d: executable="%s"`, k.PID, root, k.Executable)
	}
	testwait.Until(r.t, fmt.Sprintf("the scenario's root %d to exit after the guard's kill (the reading is its /proc status)", root), func() (string, bool) {
		return liveStatus(root), !r.scenario.alive()
	})
	_, finish := r.finishScenario()
	// The fall is measured to the reading that shows it, however long that takes; only a guard
	// that has turned off, and so reads no more, leaves it unmeasured.
	testwait.Until(r.t, fmt.Sprintf("the reading to fall, by the guard's own rule, after its kill of pid %d at the reading %d, or the guard to turn off (the reading is the guard's watch)",
		k.PID, k.ReadingBytes), func() (string, bool) {
		first, _ := r.watch.firstKill()
		return r.watch.reading(), first.fallen || r.guard.isDead()
	})
	k, _ = r.watch.firstKill()
	fall := time.Duration(-1)
	if k.fallen {
		fall = k.fall
	}
	r.out.printf("killed pid=%d reading_at_kill=%d share_at_kill=%.2f%% decision_ms=%s fall_ms=%s longest_interval_ms=%s max_rise_per_sample=%d size=%d kills=%d",
		k.PID, k.ReadingBytes, 100*float64(k.ReadingBytes)/float64(k.LimitBytes), liveMS(k.decision), liveMS(fall),
		liveMS(r.watch.longestInterval()), r.sampler.maxRise, k.SizeBytes, r.watch.kills())
	switch {
	case !liveSIGKILL(r.scenario.state):
		return fmt.Sprintf("the scenario's root %d ended with %s and not by SIGKILL", root, r.scenario.state)
	case !r.holder.alive():
		return fmt.Sprintf("the holder is dead (%s)", r.holder.state)
	}
	return finish
}

// awaitFirstKill waits for the guard's first kill, with no bound of its own, and answers why
// there is none: the guard turned off, the scenario ended by itself, or a SIGKILL that was not the
// guard's ended it.
func (r *liveRun) awaitFirstKill() (watchedKill, string) {
	testwait.Until(r.t, "the guard's first kill, the scenario's end or the guard's turning off (the reading is the guard's watch and the scenario's last sample)", func() (string, bool) {
		_, killed := r.watch.firstKill()
		return r.watch.reading() + "; " + r.sampler.latestLine(), killed || !r.scenario.alive() || r.guard.isDead()
	})
	if k, ok := r.watch.firstKill(); ok {
		return k, ""
	}
	if r.scenario.alive() {
		// A scenario still running ended the wait only by the guard's turning off.
		r.finishScenario()
		return watchedKill{}, liveGuardOffReason
	}
	if !liveSIGKILL(r.scenario.state) {
		r.finishScenario()
		return watchedKill{}, fmt.Sprintf("the scenario ended by itself (%s) and the guard ended nothing", r.scenario.state)
	}
	// The guard may have sent that SIGKILL and not reported it yet.
	r.awaitNextReading()
	if k, ok := r.watch.firstKill(); ok {
		return k, ""
	}
	r.finishScenario()
	if r.guard.isDead() {
		return watchedKill{}, liveGuardOffReason
	}
	return watchedKill{}, "the scenario ended by SIGKILL and the guard reported no kill by its next reading: the SIGKILL was not the guard's"
}

// awaitNextReading waits for a reading the guard starts after this call, or for the guard to turn
// off. The guard reports a kill on the loop that made it, before it starts its next reading, so
// a kill it made before this call is reported by then: the proof, with no span held for the
// report, that a SIGKILL that ended the scenario before the call was not the guard's.
func (r *liveRun) awaitNextReading() {
	seen := r.watch.readingCount()
	testwait.Until(r.t, fmt.Sprintf("a reading the guard starts after its reading %d, or the guard to turn off (the reading is the guard's watch)", seen), func() (string, bool) {
		return r.watch.reading(), r.watch.readingCount() > seen || r.guard.isDead()
	})
}

// awaitKept waits for the scenario to end by itself or to outlive the span the run asks it be
// kept over, and passes when it ended with status 0 or was alive at the span's end, the guard
// ended nothing, and the holder is alive.
//
// The span is not a wait for a fact. It is the span the run names (liveSecondsEnv) for the guard
// to keep the scenario over, which the harness spends as it spends the CPU window: the samples
// over it are the run's record, and that the guard ends nothing in it is the verdict. Only the
// scenario's own end closes it early, since neither the guard's state nor a later action proves
// that a scenario still running will not be ended later in the span.
func (r *liveRun) awaitKept() string {
	span := time.NewTimer(r.cfg.bound)
	defer span.Stop()
	for waiting := true; waiting; {
		if k, ok := r.watch.firstKill(); ok {
			r.finishScenario()
			return liveKeptKillReason(k)
		}
		select {
		case <-r.watch.notify:
		case <-r.scenario.done:
			waiting = false
		case <-span.C:
			waiting = false
		case <-r.ctx.Done():
			return liveDeadlineReason
		}
	}
	byTest, finish := r.finishScenario()
	if !byTest && liveSIGKILL(r.scenario.state) {
		// The guard may have sent that SIGKILL and not reported it yet.
		r.awaitNextReading()
	}
	if k, ok := r.watch.firstKill(); ok {
		return liveKeptKillReason(k)
	}
	switch {
	case !byTest && !r.scenario.state.Success():
		return fmt.Sprintf("the scenario ended by itself with %s and not status 0", r.scenario.state)
	case !r.holder.alive():
		return fmt.Sprintf("the holder is dead (%s)", r.holder.state)
	case r.guard.isDead():
		return liveGuardOffReason
	case finish != "":
		return finish
	}
	r.out.printf("kept highest_sample=%s max_rise_per_sample=%d", r.sampler.highestField(), r.sampler.maxRise)
	return ""
}

func liveKeptKillReason(k watchedKill) string {
	return fmt.Sprintf(`the guard ended pid %d: executable="%s"`, k.PID, k.Executable)
}

// liveSIGKILL is whether state is a process ended by SIGKILL.
func liveSIGKILL(state *os.ProcessState) bool {
	status, ok := state.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// finishScenario ends the scenario's process group, whatever of it still runs, stops the sampler
// and prints the end line, once. byTest is the scenario's root still running when the test ended
// it, and reason is why the run's samples cannot be trusted, or "".
func (r *liveRun) finishScenario() (byTest bool, reason string) {
	if r.finished {
		return false, ""
	}
	r.finished = true
	byTest = r.scenario.alive()
	r.scenario.end(r.t)
	close(r.samplerStop)
	testwait.Receive(r.t, "the sampler to stop", r.samplerDone)
	s := r.sampler
	r.out.printf("end pid=%d status=%q ended_by_test=%t samples=%d highest_sample=%s max_rise_per_sample=%d",
		r.scenario.cmd.Process.Pid, r.scenario.state.String(), byTest, s.samples, s.highestField(), s.maxRise)
	if s.err != nil {
		return byTest, fmt.Sprintf("a sample failed: %v", s.err)
	}
	return byTest, ""
}

// teardown ends what the run started and has not ended, before the verdict line: the scenario
// and the guard.
func (r *liveRun) teardown() {
	if r.scenario != nil {
		r.finishScenario()
	}
	if r.stopGuard != nil {
		r.stopGuard()
	}
}

// liveChild is a process the live test started, its output relayed as the test's own lines and
// its exit watched on a goroutine of its own.
type liveChild struct {
	// name is what its relayed lines start with: holder or scenario.
	name string
	cmd  *exec.Cmd
	// group is the child leading a process group of its own, which end kills whole.
	group bool
	done  chan struct{}
	// state is the child's exit, set before done closes.
	state *os.ProcessState
}

// startLiveChild starts cmd with its stdout and stderr relayed as lines that start with name,
// each also handed to onLine when it is set. The child holds the pipe's write end alone, so Wait
// does not wait for the relay, nor for a grandchild that keeps the pipe open.
func startLiveChild(out *liveLog, name string, cmd *exec.Cmd, onLine func(string)) (*liveChild, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = write, write
	err = cmd.Start()
	_ = write.Close()
	if err != nil {
		_ = read.Close()
		return nil, err
	}
	go relayLines(out, name, read, onLine)
	c := &liveChild{name: name, cmd: cmd, group: cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		c.state = cmd.ProcessState
		close(c.done)
	}()
	return c, nil
}

// relayLines prints each line of r as the test's own, escaped and cut to liveRelayLineBytes. A
// line longer than the scanner holds ends the relay, and the rest is read and dropped so the child
// never blocks on a full pipe.
func relayLines(out *liveLog, name string, r *os.File, onLine func(string)) {
	defer r.Close()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		out.printf("%s: %s", name, escaped(line, liveRelayLineBytes))
		if onLine != nil {
			onLine(line)
		}
	}
	if err := scanner.Err(); err != nil {
		out.printf("%s: no more of its output is relayed: %v", name, err)
		_, _ = io.Copy(io.Discard, r)
	}
}

func (c *liveChild) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// end sends SIGKILL to the child, or to its whole process group when it leads one, and waits for
// the child to exit.
func (c *liveChild) end(t *testing.T) {
	if c.group {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	} else if c.alive() {
		_ = c.cmd.Process.Kill()
	}
	pid := c.cmd.Process.Pid
	testwait.Until(t, fmt.Sprintf("the %s %d to exit after SIGKILL (the reading is its /proc status)", c.name, pid), func() (string, bool) {
		return liveStatus(pid), !c.alive()
	})
}

// guardWatch is what the test learns from the guard's own loop: each reading through the guard's
// observe hook and each kill through its Sink, both called on that loop and read by the test's
// goroutine under mu. notify wakes the test at each kill and at each kill's fall.
type guardWatch struct {
	act    int
	notify chan struct{}

	mu sync.Mutex
	// readings counts the guard's readings; last, lastReading and lastLimit are the latest one's
	// time, AnonPages and limit, and longest the longest time from one reading to the next over
	// the whole run.
	readings    int
	last        time.Time
	lastReading uint64
	lastLimit   uint64
	longest     time.Duration
	// riseStart is the time of the first reading at or above the mark since the reading was last
	// below it: the start of the rise a kill decides on.
	riseStart time.Time
	killed    []watchedKill
}

// watchedKill is one kill the guard reported, with the time from its rise's first reading at or
// above the mark to the kill (decision, below 0 when none was seen), and from the kill to the
// first reading that has fallen by the guard's own rule (fall, once fallen).
type watchedKill struct {
	Kill
	decision time.Duration
	fallen   bool
	fall     time.Duration
}

func (w *guardWatch) observe(now time.Time, reading, limit uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.last.IsZero() {
		w.longest = max(w.longest, now.Sub(w.last))
	}
	w.readings++
	w.last, w.lastReading, w.lastLimit = now, reading, limit
	mark := share(limit, w.act)
	switch {
	case reading < mark:
		w.riseStart = time.Time{}
	case w.riseStart.IsZero():
		w.riseStart = now
	}
	for i := range w.killed {
		k := &w.killed[i]
		// The guard's own rule (fell): below the reading at the kill by any amount, or below the
		// mark.
		if !k.fallen && fell(k.ReadingBytes, reading, mark) {
			k.fallen, k.fall = true, now.Sub(k.At)
			w.poke()
		}
	}
}

func (w *guardWatch) sink(k Kill) {
	w.mu.Lock()
	defer w.mu.Unlock()
	decision := time.Duration(-1)
	if !w.riseStart.IsZero() {
		decision = k.At.Sub(w.riseStart)
	}
	w.killed = append(w.killed, watchedKill{Kill: k, decision: decision})
	w.poke()
}

func (w *guardWatch) poke() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// firstKill is the first kill the guard reported, as the watch knows it now.
func (w *guardWatch) firstKill() (watchedKill, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.killed) == 0 {
		return watchedKill{}, false
	}
	return w.killed[0], true
}

func (w *guardWatch) kills() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.killed)
}

func (w *guardWatch) longestInterval() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.longest
}

// readingCount is how many readings the guard has taken.
func (w *guardWatch) readingCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.readings
}

// reading is what the watch knows now, as a wait on the guard prints it when it gives up: the
// guard's readings so far, the latest one with the mark it was judged by, and the kills reported.
func (w *guardWatch) reading() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.readings == 0 {
		return "the guard has taken no reading"
	}
	return fmt.Sprintf("guard readings=%d last_reading=%d mark=%d last_at=%s kills=%d",
		w.readings, w.lastReading, share(w.lastLimit, w.act), liveTime(w.last), len(w.killed))
}

// liveSampler prints one sample line each liveSampleEvery while the scenario runs, and keeps the
// highest sample by AnonPages and the largest rise of AnonPages from one sample to the next. Its
// fields are the sampler's goroutine's until that has stopped, but latest, the line of its latest
// sample, which it publishes for a wait's reading (latestLine).
type liveSampler struct {
	out   *liveLog
	root  int
	smaps []byte
	// memfds are the memfds a sample's sizes have counted (addHeldMemfds), emptied at each
	// sample.
	memfds map[memfdID]bool

	samples     int
	last        uint64
	highest     uint64
	highestText string
	maxRise     uint64
	err         error
	latest      atomic.Pointer[string]
}

func (s *liveSampler) run(ended, stop <-chan struct{}) {
	tick := time.NewTicker(liveSampleEvery)
	defer tick.Stop()
	for {
		if err := s.take(); err != nil {
			s.err = err
			s.out.printf("sample failed reason=%v", err)
			return
		}
		select {
		case <-ended:
			return
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

// take prints one sample: the pod's AnonPages, the cgroup's counter and Shmem, and the
// scenario's largest process (largest).
func (s *liveSampler) take() error {
	at := time.Now()
	anon, shmem, err := liveMeminfo()
	if err != nil {
		return err
	}
	counter, err := liveCount(filepath.Join(liveCgroup, "memory.usage_in_bytes"))
	if err != nil {
		return err
	}
	top := s.largest(shmem)
	text := fmt.Sprintf("sample anon_pages=%d counter=%d shmem=%d pid=%d anonymous=%d size=%d vm_rss=%d at=%s",
		anon, counter, shmem, top.pid, top.anonymous, top.size, top.vmRSS, liveTime(at))
	s.out.write(text)
	s.latest.Store(&text)
	if s.samples > 0 && anon > s.last {
		s.maxRise = max(s.maxRise, anon-s.last)
	}
	if s.samples == 0 || anon > s.highest {
		s.highest, s.highestText = anon, text
	}
	s.last = anon
	s.samples++
	return nil
}

// highestField is the highest sample's whole line, quoted, or none before the first sample.
func (s *liveSampler) highestField() string {
	if s.samples == 0 {
		return "none"
	}
	return `"` + s.highestText + `"`
}

// latestLine is the line of the latest sample, or says there is none yet; any goroutine may call
// it.
func (s *liveSampler) latestLine() string {
	if text := s.latest.Load(); text != nil {
		return *text
	}
	return "no sample yet"
}

// liveProcess is one of the scenario's processes as a sample prints it: its smaps' Anonymous
// sum, its size by the guard's own rule (smapsSize and addHeldMemfds), and its VmRSS.
type liveProcess struct {
	pid                    int
	anonymous, size, vmRSS uint64
}

// largest is the scenario's largest process by the guard's own rule, among its root and the
// root's descendants by their PPid chain: each one's smaps size, and the memfds it holds open by
// a descriptor that none of them maps and no larger one of them holds, each at its size or shmem,
// the sample's Shmem reading, whichever is less, ranked as the guard ranks them (addHeldMemfds),
// so a process that grows a memfd it never maps shows that memfd's size up to Shmem. pid 0 when
// none could be read.
func (s *liveSampler) largest(shmem uint64) liveProcess {
	if s.memfds == nil {
		s.memfds = map[memfdID]bool{}
	}
	clear(s.memfds)
	var read []candidate
	anonymous := map[int]uint64{}
	for _, p := range liveDescendants(s.root) {
		data, err := readAll(filepath.Join(liveProc, strconv.Itoa(p.PID), "smaps"), s.smaps)
		s.smaps = data
		if err != nil {
			continue
		}
		read = append(read, candidate{Process: p, size: smapsSize(data, s.memfds)})
		anonymous[p.PID] = smapsAnonymous(data)
	}
	if len(read) == 0 {
		return liveProcess{}
	}
	addHeldMemfds(read, liveProc, memfdPrefix, s.memfds, shmem)
	top := liveProcess{pid: read[0].PID, anonymous: anonymous[read[0].PID], size: read[0].size}
	if status, err := os.ReadFile(filepath.Join(liveProc, strconv.Itoa(top.pid), "status")); err == nil {
		top.vmRSS, _ = meminfoBytes(status, "VmRSS")
	}
	return top
}

// liveDescendants is root and every process whose PPid chain leads to it, each as its stat reads
// (the root by its pid alone where its stat cannot be read).
func liveDescendants(root int) []*Process {
	procs := map[int]*Process{root: {PID: root}}
	entries, err := os.ReadDir(liveProc)
	if err != nil {
		return []*Process{procs[root]}
	}
	children := map[int][]int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(liveProc, entry.Name(), "stat"))
		if err != nil {
			continue
		}
		if p, err := parseStat(stat); err == nil {
			p.PID = pid
			procs[pid] = p
			children[p.PPID] = append(children[p.PPID], pid)
		}
	}
	seen := map[int]bool{root: true}
	family := []*Process{procs[root]}
	for i := 0; i < len(family); i++ {
		for _, child := range children[family[i].PID] {
			if !seen[child] {
				seen[child] = true
				family = append(family, procs[child])
			}
		}
	}
	return family
}

// smapsAnonymous is the sum of a smaps' Anonymous lines alone, which a sample prints beside the
// guard's size so a reader sees what the shared mappings and the memfds held by a descriptor
// added.
func smapsAnonymous(data []byte) uint64 {
	var sum uint64
	for line := range bytes.Lines(data) {
		if rest, ok := bytes.CutPrefix(line, []byte("Anonymous:")); ok {
			if n, ok := parseKB(rest); ok {
				sum += n
			}
		}
	}
	return sum
}

// liveMeminfo is the pod's AnonPages and Shmem from /proc/meminfo, in bytes.
func liveMeminfo() (anon, shmem uint64, err error) {
	path := filepath.Join(liveProc, "meminfo")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var ok bool
	if anon, ok = meminfoBytes(data, "AnonPages"); !ok {
		return 0, 0, errors.New("no AnonPages line in " + path)
	}
	shmem, _ = meminfoBytes(data, "Shmem")
	return anon, shmem, nil
}

// liveStatus is the state and resident size /proc/<pid>/status gives for pid, the reading of a
// wait on a process the live test started, or why it cannot be read (a reaped process has none).
func liveStatus(pid int) string {
	data, err := os.ReadFile(filepath.Join(liveProc, strconv.Itoa(pid), "status"))
	if err != nil {
		return err.Error()
	}
	state := "unknown"
	for line := range bytes.Lines(data) {
		if rest, ok := bytes.CutPrefix(line, []byte("State:")); ok {
			state = string(bytes.TrimSpace(rest))
			break
		}
	}
	rss, _ := meminfoBytes(data, "VmRSS")
	return fmt.Sprintf("pid=%d state=%q vm_rss=%d", pid, state, rss)
}

// liveCount is the one number the file at path holds, such as the cgroup's limit or counter.
func liveCount(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, ok := parseCount(data)
	if !ok {
		return 0, fmt.Errorf("%s reads %q", path, bytes.TrimSpace(data))
	}
	return n, nil
}

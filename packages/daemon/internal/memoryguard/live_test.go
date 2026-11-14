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
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
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
	// liveSecondsEnv bounds the scenario: a kept scenario still alive after it has been kept, and a
	// scenario to be ended that the guard has not ended by then fails.
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
	// liveStartBound bounds the holder's start: writing 256 MiB takes about a second.
	liveStartBound = time.Minute
	// liveExitBound bounds how long a process sent SIGKILL takes to exit.
	liveExitBound = time.Minute
	// liveReportGrace is how long the test waits for the guard's report of a kill once the
	// scenario has exited by SIGKILL: the guard reads the cgroup's counter between its kill and
	// its report.
	liveReportGrace = 5 * time.Second
	// liveFallBound bounds the wait for the reading to fall after a kill: the longest wait the
	// settings allow, so a fall slower than the guard's own wait is still measured.
	liveFallBound = shimwire.MemoryGuardWaitMax * time.Second
	// liveDeadlineMargin is what the test leaves of the test binary's deadline for its own end, so
	// a -timeout shorter than the run ends in a verdict line and not in the binary's panic.
	liveDeadlineMargin = 15 * time.Second
	// liveRelayLineBytes bounds one relayed line of the holder's or the scenario's output, so each
	// is one write that no other line splits.
	liveRelayLineBytes = 1024
	// liveStamp is RFC 3339 with milliseconds, the time every line carries.
	liveStamp = "2006-01-02T15:04:05.000Z07:00"

	liveDeadlineReason = "the test binary's deadline came first: run with a longer -timeout"
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
	if reason := r.run(); reason != "" {
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
	settings  Settings
	wire      *shimwire.LauncherMemoryGuard
	run       string
	expect    string
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
		c.settings, _ = SettingsOf(&w)
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
	ctx, cancel := liveDeadline(r.t)
	defer cancel()
	r.ctx = ctx
	defer r.teardown()
	if reason := r.printStart(); reason != "" {
		return reason
	}
	if reason := r.startHelpers(); reason != "" {
		return reason
	}
	if cfg.guardOn {
		if reason := r.startGuard(); reason != "" {
			return reason
		}
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

// liveDeadline is a context that ends liveDeadlineMargin before the test binary's own deadline.
func liveDeadline(t *testing.T) (context.Context, context.CancelFunc) {
	if deadline, ok := t.Deadline(); ok {
		return context.WithDeadline(context.Background(), deadline.Add(-liveDeadlineMargin))
	}
	return context.WithCancel(context.Background())
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
	r.t.Cleanup(func() { holder.end() })
	select {
	case <-ready:
	case <-holder.done:
		return fmt.Sprintf("the holder ended (%s) before it held its memory", holder.state)
	case <-time.After(liveStartBound):
		return fmt.Sprintf("the holder did not hold its memory within %s", liveStartBound)
	case <-r.ctx.Done():
		return liveDeadlineReason
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
func (r *liveRun) startGuard() string {
	r.watch = &guardWatch{act: r.cfg.settings.ActPercent, notify: make(chan struct{}, 1)}
	g, err := New(Config{
		Sink: r.watch.sink,
		Logf: func(format string, args ...any) { r.out.printf("guard: %s", fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		return err.Error()
	}
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
		<-done
	})
	return ""
}

// measureCPU runs the guard alone for the CPU window, with no scenario and no sampling, and
// prints the CPU time the test process used over it: the guard's own plus that of the idle rest of
// the test process (the Go runtime, and the goroutines that wait on the holder and relay its
// output), which one process cannot tell apart. The window starts as the guard does, so its first
// reading, and the scan for the hourly line that reading starts, count in it.
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
// first, so a scenario that ends by itself is a negative control that never reached the kill.
func (r *liveRun) awaitOff() string {
	select {
	case <-r.scenario.done:
	case <-r.ctx.Done():
		return liveDeadlineReason
	}
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
		return fmt.Sprintf(`the guard ended pid %d and not the scenario's root %d: command="%s"`, k.PID, root, k.Command)
	}
	select {
	case <-r.scenario.done:
	case <-time.After(liveExitBound):
	case <-r.ctx.Done():
		return liveDeadlineReason
	}
	exited := !r.scenario.alive()
	_, finish := r.finishScenario()
	r.watch.await(r.ctx, time.Until(k.At.Add(liveFallBound)), func() bool {
		first, _ := r.watch.firstKill()
		return first.fallen
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
	case !exited:
		return fmt.Sprintf("the scenario's root %d had not exited %s after the guard's kill", root, liveExitBound)
	case !liveSIGKILL(r.scenario.state):
		return fmt.Sprintf("the scenario's root %d ended with %s and not by SIGKILL", root, r.scenario.state)
	case !r.holder.alive():
		return fmt.Sprintf("the holder is dead (%s)", r.holder.state)
	}
	return finish
}

// awaitFirstKill waits for the guard's first kill, and answers why there is none: the scenario
// ended by itself, the bound passed, or the guard turned off.
func (r *liveRun) awaitFirstKill() (watchedKill, string) {
	bound := time.NewTimer(r.cfg.bound)
	defer bound.Stop()
	scenarioDone := r.scenario.done
	var grace <-chan time.Time
	for {
		if k, ok := r.watch.firstKill(); ok {
			return k, ""
		}
		select {
		case <-r.watch.notify:
		case <-scenarioDone:
			scenarioDone = nil
			if !liveSIGKILL(r.scenario.state) {
				r.finishScenario()
				return watchedKill{}, fmt.Sprintf("the scenario ended by itself (%s) and the guard ended nothing", r.scenario.state)
			}
			grace = time.After(liveReportGrace)
		case <-grace:
			r.finishScenario()
			return watchedKill{}, fmt.Sprintf("the scenario ended by SIGKILL and the guard reported no kill within %s", liveReportGrace)
		case <-bound.C:
			r.finishScenario()
			if r.guard.isDead() {
				return watchedKill{}, liveGuardOffReason
			}
			return watchedKill{}, fmt.Sprintf("the guard ended nothing within %ds", int(r.cfg.bound/time.Second))
		case <-r.ctx.Done():
			return watchedKill{}, liveDeadlineReason
		}
	}
}

// awaitKept waits for the scenario to end by itself or to outlive the bound, and passes when it
// ended with status 0 or was alive at the bound, the guard ended nothing, and the holder is alive.
func (r *liveRun) awaitKept() string {
	bound := time.NewTimer(r.cfg.bound)
	defer bound.Stop()
	for waiting := true; waiting; {
		if k, ok := r.watch.firstKill(); ok {
			r.finishScenario()
			return liveKeptKillReason(k)
		}
		select {
		case <-r.watch.notify:
		case <-r.scenario.done:
			waiting = false
		case <-bound.C:
			waiting = false
		case <-r.ctx.Done():
			return liveDeadlineReason
		}
	}
	byTest, finish := r.finishScenario()
	if r.scenario.alive() {
		return finish
	}
	if !byTest && liveSIGKILL(r.scenario.state) {
		r.watch.await(r.ctx, liveReportGrace, func() bool {
			_, ok := r.watch.firstKill()
			return ok
		})
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
	return fmt.Sprintf(`the guard ended pid %d: command="%s"`, k.PID, k.Command)
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
	exited := r.scenario.end()
	close(r.samplerStop)
	<-r.samplerDone
	status := "running"
	if exited {
		status = r.scenario.state.String()
	}
	s := r.sampler
	r.out.printf("end pid=%d status=%q ended_by_test=%t samples=%d highest_sample=%s max_rise_per_sample=%d",
		r.scenario.cmd.Process.Pid, status, byTest, s.samples, s.highestField(), s.maxRise)
	switch {
	case !exited:
		return byTest, fmt.Sprintf("the scenario's root had not exited %s after SIGKILL", liveExitBound)
	case s.err != nil:
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
	cmd *exec.Cmd
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
	c := &liveChild{cmd: cmd, group: cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid, done: make(chan struct{})}
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
		var b strings.Builder
		escape(&b, line)
		out.printf("%s: %s", name, cut(b.String(), liveRelayLineBytes))
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

// end sends SIGKILL to the child, or to its whole process group when it leads one, and answers
// whether the child has exited within liveExitBound.
func (c *liveChild) end() bool {
	if c.group {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	} else if c.alive() {
		_ = c.cmd.Process.Kill()
	}
	select {
	case <-c.done:
		return true
	case <-time.After(liveExitBound):
		return false
	}
}

// guardWatch is what the test learns from the guard's own loop: each reading through the guard's
// observe hook and each kill through its Sink, both called on that loop and read by the test's
// goroutine under mu. notify wakes the test at each kill and at each kill's fall.
type guardWatch struct {
	act    int
	notify chan struct{}

	mu sync.Mutex
	// last is the latest reading's time, and longest the longest time from one reading to the
	// next over the whole run.
	last    time.Time
	longest time.Duration
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
	w.last = now
	mark := share(limit, w.act)
	switch {
	case reading < mark:
		w.riseStart = time.Time{}
	case w.riseStart.IsZero():
		w.riseStart = now
	}
	for i := range w.killed {
		k := &w.killed[i]
		// The guard's own rule (decide): half the killed size below the reading at the kill, or
		// below the mark.
		if !k.fallen && (reading < mark || (k.ReadingBytes >= k.SizeBytes/2 && reading <= k.ReadingBytes-k.SizeBytes/2)) {
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

// await waits until done holds, d passes or ctx ends, asking done again at each kill and fall.
func (w *guardWatch) await(ctx context.Context, d time.Duration, done func() bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for !done() {
		select {
		case <-w.notify:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// liveSampler prints one sample line each liveSampleEvery while the scenario runs, and keeps the
// highest sample by AnonPages and the largest rise of AnonPages from one sample to the next. Its
// fields are the sampler's goroutine's until that has stopped.
type liveSampler struct {
	out   *liveLog
	root  int
	smaps []byte

	samples     int
	last        uint64
	highest     uint64
	highestText string
	maxRise     uint64
	err         error
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
	top := s.largest()
	text := fmt.Sprintf("sample anon_pages=%d counter=%d shmem=%d pid=%d anonymous=%d size=%d vm_rss=%d at=%s",
		anon, counter, shmem, top.pid, top.anonymous, top.size, top.vmRSS, liveTime(at))
	s.out.write(text)
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

// liveProcess is one of the scenario's processes as a sample prints it: its smaps' Anonymous
// sum, its size by the guard's own rule (smapsSize), and its VmRSS.
type liveProcess struct {
	pid                    int
	anonymous, size, vmRSS uint64
}

// largest is the scenario's largest process by the guard's own rule, among its root and the
// root's descendants by their PPid chain; pid 0 when none could be read.
func (s *liveSampler) largest() liveProcess {
	var top liveProcess
	for _, pid := range liveDescendants(s.root) {
		dir := filepath.Join(liveProc, strconv.Itoa(pid))
		data, err := readAll(filepath.Join(dir, "smaps"), s.smaps)
		s.smaps = data
		if err != nil {
			continue
		}
		size := smapsSize(data)
		if top.pid != 0 && size <= top.size {
			continue
		}
		top = liveProcess{pid: pid, anonymous: smapsAnonymous(data), size: size}
		if status, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
			top.vmRSS, _ = meminfoBytes(status, "VmRSS")
		}
	}
	return top
}

// liveDescendants is root and every process whose PPid chain leads to it, read from each
// process's stat.
func liveDescendants(root int) []int {
	entries, err := os.ReadDir(liveProc)
	if err != nil {
		return []int{root}
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
			children[p.PPID] = append(children[p.PPID], pid)
		}
	}
	seen := map[int]bool{root: true}
	family := []int{root}
	for i := 0; i < len(family); i++ {
		for _, child := range children[family[i]] {
			if !seen[child] {
				seen[child] = true
				family = append(family, child)
			}
		}
	}
	return family
}

// smapsAnonymous is the sum of a smaps' Anonymous lines alone, which a sample prints beside the
// guard's size so a reader sees what the shared mappings added.
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

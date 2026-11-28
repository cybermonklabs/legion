// packages/envoy/cmd/cipher/children.go
//
// The one tracker of every child process the secret forms start: gcloud today, and any tool a
// store runs. Each store reaches it as the secretstore.RunChild its writer is opened with.
package main

import (
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// childWaitDelay is how long runChild waits for a killed child's output to close before it gives
// the call up, where the caller set no WaitDelay of its own: a second.
const childWaitDelay = time.Second

// runningChildren holds the pid of each child runChild has started and not yet waited for.
var runningChildren = struct {
	sync.Mutex
	pids map[int]bool
}{pids: map[int]bool{}}

// formWatchesSignals counts the form watches of createSignals running now (watchCreateSignals),
// each of which ends the form by a signal through exitBySignal itself.
var formWatchesSignals atomic.Int32

// childStarted and childWaited name the two moments of runChild at which onChildStep runs.
const (
	childStarted = "started" // the child has started; its pid is not recorded yet
	childWaited  = "waited"  // the child has been waited for; what it left is not killed yet
)

// onChildStep runs in runChild at each of childStarted and childWaited. A test replaces it to send
// a signal at one of those moments, which no timing of the test's own can hit on every run.
var onChildStep = func(step string) {}

// runChild runs cmd as cmd.Run does, in a process group of its own where the platform has them,
// whose end of context kills that whole group, not cmd's process alone (killGroupOnCancel); cmd
// comes from exec.CommandContext, since exec refuses a Cancel on any other. The tool on PATH can be
// a script whose child holds the output open and outlives a kill of the script. It waits for the
// output no more than childWaitDelay, where the caller set no WaitDelay,
// for a process that left the group. Its pid is recorded from its start until it has been waited
// for, so an end by a signal (exitBySignal) kills it with its group: in a group of its own, the
// child gets no signal sent to this command's group, such as a terminal's interrupt. Where no form
// watches createSignals, it watches them itself (watchSignalsWhileChildRuns) from before cmd
// starts until what cmd left is killed, so that no signal in that span takes its default action
// and ends this process with cmd's group still running; one caught while cmd starts is handled
// once cmd's pid is recorded, since killRunningChildren waits on runningChildren's lock. Once cmd
// has been waited for, on every outcome, it kills what is left of cmd's group (killGroupLeft): a
// child that exits by itself can leave one of its own holding its output, which the wait delay
// stops the wait for and does not kill. The group is cmd's own, so nothing else is in it, and
// while a process is left in it its id names no other group. It is the secretstore.RunChild every
// writer this command opens is given.
// It runs on one OS thread from before cmd starts until cmd has been waited for: where the
// child's parent-death signal is set (childProcAttr), the kernel sends it when the thread that
// started the child ends, not this process, and the Go runtime ends a thread only when a
// goroutine locked to it returns locked.
func runChild(cmd *exec.Cmd) error {
	killGroupOnCancel(cmd)
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = childWaitDelay
	}
	stop := watchSignalsWhileChildRuns()
	defer stop()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	runningChildren.Lock()
	if err := cmd.Start(); err != nil {
		runningChildren.Unlock()
		return err
	}
	pid := cmd.Process.Pid
	onChildStep(childStarted)
	runningChildren.pids[pid] = true
	runningChildren.Unlock()
	err := cmd.Wait()
	onChildStep(childWaited)
	killGroupLeft(pid)
	runningChildren.Lock()
	delete(runningChildren.pids, pid)
	runningChildren.Unlock()
	return err
}

// killRunningChildren kills each child runChild is running, with its group (killChildGroup), for
// a command that is ending by a signal. It keeps runningChildren locked, so no child starts after
// it.
func killRunningChildren() {
	runningChildren.Lock()
	for pid := range runningChildren.pids {
		killChildGroup(pid)
	}
}

// watchSignalsWhileChildRuns catches each of createSignals this process did not inherit as
// ignored, until stop is called, where no form watches them (formWatchesSignals), and ends the
// process by the first that comes (exitBySignal, which kills the running children first), as the
// signal's default action would have, once the value prompt, where it reads, has finished
// (promptBusy). stop never returns once the watch has taken a signal, since the process is ending
// by it, as watchCreateSignals' stop does; a signal that came with the stop still ends the
// process.
func watchSignalsWhileChildRuns() (stop func()) {
	if formWatchesSignals.Load() > 0 {
		return func() {}
	}
	var watched []os.Signal
	for _, sig := range createSignals {
		if !signal.Ignored(sig) {
			watched = append(watched, sig)
		}
	}
	if len(watched) == 0 {
		return func() {}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, watched...)
	end := func(sig os.Signal) {
		promptBusy.Lock()
		exitBySignal(sig)
	}
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		select {
		case sig := <-signals:
			end(sig)
		case <-done:
			close(exited)
		}
	}()
	return func() {
		close(done)
		<-exited
		signal.Stop(signals)
		select {
		case sig := <-signals:
			end(sig)
		default:
		}
	}
}

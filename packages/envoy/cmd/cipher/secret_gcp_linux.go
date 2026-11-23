//go:build linux

// packages/envoy/cmd/cipher/secret_gcp_linux.go
//
// How gcloud's process group ends with this command on Linux: under a supervisor.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
)

// gcloudSupervisorEnv marks a run of this binary as the supervisor of one gcloud call
// (superviseGcloud, runGcloudSupervisor): its arguments are gcloud's path and gcloud's own.
const gcloudSupervisorEnv = "AGENT_SECRETS_GCLOUD_SUPERVISOR"

// selfExe is the path that names this process's own executable, re-executed as gcloud's
// supervisor: the file the process runs, even where its path now names another file, as it does
// once the binary is replaced during a call (os.Executable reads the path, " (deleted)" trimmed).
const selfExe = "/proc/self/exe"

// superviseGcloud makes cmd, a gcloud command killGroupOnCancel sets up, start this binary as
// gcloud's supervisor, with gcloud's path and arguments for its own, and reports whether it did:
// the supervisor leads gcloud's process group, starts gcloud in it, and ends as gcloud ends
// (runGcloudSupervisor). Where this command ends by a SIGKILL, which runs none of its code, the
// kernel sends the supervisor the parent-death signal it starts with (gcloudProcAttr), and the
// supervisor kills the whole group, gcloud and whatever gcloud started in it, itself among them.
// gcloud alone cannot be trusted with that signal: the kernel clears it at the exec of a setuid or
// file-capability launcher, such as a snap install's /snap/bin/gcloud (snap-confine), and the
// supervisor, this same binary, has neither. gcloud still carries one of its own, SIGKILL, set by
// the supervisor, which reaches it when the supervisor ends: a launcher that moves gcloud into a
// session or process group of its own (setsid, setpgid), out of reach of a kill of the group,
// keeps that signal, unless it forks to do so or its exec clears it. The group is the
// supervisor's, so the kills at a deadline, at a signal this command handles and after the wait
// (killGroupOnCancel, killRunningGcloud, killGroupLeft) reach gcloud as before. It leaves cmd as it
// is, and reports false, for a gcloud that cannot be found (cmd.Err, which Start then answers) and
// where /proc does not name this binary; gcloud then carries the parent-death signal itself.
// A gcloud the supervisor cannot start ends the supervisor with status 127, and its reason, which
// the supervisor writes on its standard error, is lost with what runGcloud discards there: the call
// fails with "exit status 127".
func superviseGcloud(cmd *exec.Cmd) bool {
	if cmd.Err != nil {
		return false
	}
	if _, err := os.Stat(selfExe); err != nil {
		return false
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Args = append([]string{cmd.Args[0], cmd.Path}, cmd.Args[1:]...)
	cmd.Path = selfExe
	cmd.Env = append(env[:len(env):len(env)], gcloudSupervisorEnv+"=1")
	return true
}

// gcloudProcAttr starts gcloud, or its supervisor where supervised (superviseGcloud), leading a
// process group of its own (killGroupOnCancel), with a parent-death signal: SIGTERM for the
// supervisor, which handles it by killing the group, and SIGKILL for gcloud itself, as before the
// supervisor, since a gcloud may handle SIGTERM and live on. The signal follows the thread that
// started the process, not this process, so runRecorded keeps that thread from Start until the
// process has been waited for.
func gcloudProcAttr(supervised bool) *syscall.SysProcAttr {
	if supervised {
		return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	}
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// A run of this binary as a gcloud supervisor does that and nothing else, before any command: in
// the agent-secrets binary and in its test binary alike, since neither runs main or TestMain first.
// It is one only where it also leads its own process group, as superviseGcloud starts it
// (gcloudProcAttr): a marker that strays into another run's environment makes no supervisor of it,
// whose SIGTERM would then kill its caller's group.
func init() {
	if os.Getenv(gcloudSupervisorEnv) == "" || syscall.Getpgrp() != os.Getpid() {
		return
	}
	os.Exit(runGcloudSupervisor(os.Args[1:]))
}

// runGcloudSupervisor is a gcloud supervisor's whole run (superviseGcloud): it starts args, gcloud's
// path and its arguments, in its own process group with its own standard input, output and error,
// and SIGKILL as gcloud's parent-death signal, and waits for it. On SIGTERM, the parent-death
// signal its parent left it, it kills its group with SIGKILL, itself included, and its own end
// sends gcloud's signal, which reaches a gcloud that has left the group too. Otherwise it answers
// gcloud's exit status, or 128 plus the number of the signal that ended gcloud, as a shell reports
// one; it does not end by that signal itself. A gcloud it cannot start answers 127, its reason on
// the standard error. gcloud does not inherit gcloudSupervisorEnv. Its status is read only by the
// agent-secrets that started it, as runGcloud's error, never as the command's own exit
// (cipher-refgen's notAgentSecretsExits).
func runGcloudSupervisor(args []string) int {
	parentGone := make(chan os.Signal, 1)
	signal.Notify(parentGone, syscall.SIGTERM)
	_ = os.Unsetenv(gcloudSupervisorEnv)
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "agent-secrets: a gcloud supervisor needs gcloud's path")
		return 2
	}
	// gcloud's parent-death signal follows the thread that starts it, not this process, so that
	// thread must not end while gcloud runs. Run from init, as it is, this run is on the main
	// thread already, which the Go runtime keeps locked through init and never ends; the lock
	// adds nothing there. It stays as a guard should the call move out of init: then no goroutine
	// that locks this thread and returns could make the runtime end it, and send the signal.
	runtime.LockOSThread()
	gcloud := exec.Command(args[0], args[1:]...)
	gcloud.Stdin, gcloud.Stdout, gcloud.Stderr = os.Stdin, os.Stdout, os.Stderr
	gcloud.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := gcloud.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-secrets: start gcloud: %v\n", err)
		return 127
	}
	waited := make(chan struct{})
	go func() {
		_ = gcloud.Wait()
		close(waited)
	}()
	select {
	case <-parentGone:
		_ = syscall.Kill(0, syscall.SIGKILL)
		select {}
	case <-waited:
	}
	if status, ok := gcloud.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return gcloud.ProcessState.ExitCode()
}

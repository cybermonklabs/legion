// packages/envoy/cmd/agent-secrets/launcher_linux.go
//go:build linux

// nearestLauncher names the session launcher above this process: the ancestor whose job it is to
// outlive `agent-secrets register --session-id`, so the helper pins it rather than the register
// command itself, which ends as the command ends (dispatch://INFRA-3890).
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// errNoLauncher is what nearestLauncher answers when no session launcher is an ancestor of this
// process: a `register --session-id` without `--exec` run outside every `omp` tree then refuses,
// rather than pinning its own process as a session root that ends with the command.
var errNoLauncher = errors.New("no session launcher is an ancestor of this process")

// launcherComm is the /proc comm of a session launcher: an `omp` process. It is the one a real
// launcher's comm reads (measured on a developer box: `comm=omp`, `exe=.../omp`).
const launcherComm = "omp"

// maxLauncherHops bounds the parent walk, as the helper's own ancestry walk is bounded: a
// pathological tree cannot spin the command, and a launcher may sit well above its caller.
const maxLauncherHops = 64

// nearestLauncher is the pid of the nearest ancestor whose /proc comm is `omp`, walking parents
// from this process's own parent. It answers errNoLauncher at pid 1 with none found, and any
// read error it meets, so the caller refuses rather than pinning the wrong process. It is a var
// so a test can set the ancestor it means without a real `omp` above it.
var nearestLauncher = func() (int, error) {
	pid := os.Getppid()
	for range maxLauncherHops {
		if pid <= 1 {
			return 0, errNoLauncher
		}
		comm, parent, err := procCommAndParent(pid)
		if err != nil {
			return 0, err
		}
		if comm == launcherComm {
			return pid, nil
		}
		pid = parent
	}
	return 0, fmt.Errorf("the ancestry of this process runs past %d processes with no %s launcher", maxLauncherHops, launcherComm)
}

// procCommAndParent reads pid's comm and parent pid from /proc/<pid>/stat. A process controls its
// comm and may put ')' and spaces in it, so the fields after the comm begin after the LAST ')'
// (proc(5); the helper's own peer.go reads it the same way).
func procCommAndParent(pid int) (comm string, parent int, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, err
	}
	stat := string(data)
	open := strings.IndexByte(stat, '(')
	closeIdx := strings.LastIndexByte(stat, ')')
	if open < 0 || closeIdx < open+1 || closeIdx+2 > len(stat) {
		return "", 0, fmt.Errorf("/proc/%d/stat has no comm", pid)
	}
	comm = stat[open+1 : closeIdx]
	fields := strings.Fields(stat[closeIdx+2:])
	if len(fields) < 2 {
		return "", 0, fmt.Errorf("/proc/%d/stat has %d fields after its comm", pid, len(fields))
	}
	parent, err = strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, fmt.Errorf("/proc/%d/stat names the parent %q: %w", pid, fields[1], err)
	}
	return comm, parent, nil
}

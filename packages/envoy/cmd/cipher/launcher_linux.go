// packages/envoy/cmd/cipher/launcher_linux.go
//go:build linux

// nearestLauncher names the session launcher above this process: the ancestor whose job it is to
// outlive `agent-secrets register --session-id`, so the helper pins it rather than the register
// command itself, which ends as the command ends. isAncestor checks that
// the root the helper answers is above this process.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// errNoLauncher is what nearestLauncher answers when no session launcher is an ancestor of this
// process: a `register --session-id` without `--exec` then names no root, and the helper refuses
// it unless the caller is inside a session already.
var errNoLauncher = errors.New("no session launcher is an ancestor of this process")

// launcherComm is the /proc comm of a session launcher: an `omp` process. It is the one a real
// launcher's comm reads (measured on a developer box: `comm=omp`, `exe=.../omp`), and the one
// the helper admits as a root (internal/cipher/helper's launcherComm).
const launcherComm = "omp"

// maxLauncherHops is the helper's bound on its ancestry walk (maxAncestryHops there), which counts
// the caller itself as the first of its 64 processes: a root the helper can reach is at most 63
// parents up, so this side walks no further, and a pathological tree cannot spin the command.
const maxLauncherHops = 64

// nearestLauncher is the pid of the nearest ancestor whose /proc comm is `omp` (walkAncestors). It
// answers errNoLauncher when it finds none, and any read error it meets; the caller then names no
// root. It is a var so a test can set the ancestor it means without a real `omp` above it.
var nearestLauncher = func() (int, error) {
	pid, found, err := walkAncestors(func(_ int, comm string) bool { return comm == launcherComm })
	if err == nil && !found {
		return 0, errNoLauncher
	}
	return pid, err
}

// isAncestor reports whether root is a strict ancestor of this process (walkAncestors), which is
// what a root the helper answers for a register without --exec must be: this process ends with
// the command, and a process not above it is another session's.
func isAncestor(root int) (bool, error) {
	_, found, err := walkAncestors(func(pid int, _ string) bool { return pid == root })
	return found, err
}

// walkAncestors is the nearest strict ancestor of this process that match holds for, by its pid
// and /proc comm, walking parents from this process's own parent as far as the helper's walk
// reaches (maxLauncherHops). It answers no match at pid 1 or 0, and an error at a parent it cannot
// read or past that depth.
func walkAncestors(match func(pid int, comm string) bool) (int, bool, error) {
	pid := os.Getppid()
	for range maxLauncherHops - 1 {
		if pid <= 1 {
			return 0, false, nil
		}
		comm, parent, err := procCommAndParent(pid)
		if err != nil {
			return 0, false, err
		}
		if match(pid, comm) {
			return pid, true, nil
		}
		pid = parent
	}
	return 0, false, fmt.Errorf("the ancestry of this process runs past the %d processes the helper's walk reaches", maxLauncherHops-1)
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

// nearestHarness answers which of names the nearest ancestor of this process runs as, by its
// /proc comm read as harnessName reads it, or "" when walkAncestors meets none of them, cannot
// read a parent, or runs past its depth. A variable so a test can stand in for the process tree.
var nearestHarness = func(names ...string) string {
	found := ""
	_, _, _ = walkAncestors(func(_ int, comm string) bool {
		name := harnessName(comm)
		for _, wanted := range names {
			if name == wanted {
				found = name
				return true
			}
		}
		return false
	})
	return found
}

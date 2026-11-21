// Package ompbroker ends the named bash services one Legion claim's Oh My Pi started through its
// worker-daemon broker, which outlive the claim's process on a runtime that gives a claim no
// namespace of its own (R5: a `wait` on a named service of a relaunched claim is cut, and the
// service stops with the process).
//
// Oh My Pi (launch/services.ts and launch/broker.ts at the pin the workers run) runs a session's
// named service through a broker, `omp __omp_worker_daemon_broker`, one per state root and project
// directory, which every session of that scope shares. The broker runs in a session of its own
// and starts each service in a session of its own, with the asking session's shell environment.
// It stops a service only when asked, when the service exits, or when it shuts down itself, which
// it does once no client is left for its idle grace; nothing stops the services of a session whose
// process ended, and a resumed session, which keeps its session id, takes them over.
//
// In an issue pod a role's processes end with the role's PID namespace, the broker and its
// services included (internal/launcher's role-init), and in the controller's pod with the
// container's PID namespace (the launcher's cleanExitedGeneration as PID 1). The tmux runtime ends
// a pane's process group, which neither the broker nor its services are in, so it ends the
// claim's services itself (End), at each stop and before each launch, which also ends the services
// of a process that died. The broker is never signalled: other sessions of the scope, the
// operator's own among them, may be using it.
//
// A service is the claim's when its parent is a broker and its environment, inherited from the
// claim's pane, names the claim — LEGION_STATE_DIR the daemon's state directory, and LEGION_PROJECT,
// LEGION_ISSUE and LEGION_ROLE the claim's token — so another claim's services, another daemon's
// and the operator's own are never touched.
package ompbroker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// BrokerArg is the argument that makes an Oh My Pi process a worker-daemon broker.
const BrokerArg = "__omp_worker_daemon_broker"

// pollInterval is how often End reads whether a process it signalled has gone.
const pollInterval = 20 * time.Millisecond

// Claim names the claim whose services End ends: the daemon's state directory, which its panes are
// told as LEGION_STATE_DIR, and the claim's token. Before, when set, bounds them to the services an
// older generation of the claim started (their LEGION_GENERATION below it), so a launch of
// generation Before ends only what earlier generations left.
type Claim struct {
	StateDir string
	Token    claim.Token
	Before   uint64
}

// Ended is one service End ended: its pid, the generation that started it, its broker's pid, and
// whether it outlived its SIGTERM for the grace and was killed.
type Ended struct {
	Pid        int
	Generation uint64
	Broker     int
	Killed     bool
}

// End ends every running process whose parent is an Oh My Pi broker and whose environment names
// c: SIGTERM to the service's process group (to the process alone where it leads no group of its
// own), and SIGKILL once grace has passed with it still running. It answers what it ended; a
// process End cannot read is not one it can tell is the claim's, and is left.
func End(c Claim, grace time.Duration) ([]Ended, error) {
	if c.StateDir == "" || c.Token == "" {
		return nil, errors.New("ompbroker: a claim is named by its state directory and its token")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	brokers := map[int]bool{}
	isBroker := func(pid int) bool {
		known, ok := brokers[pid]
		if !ok {
			known = hasArg(pid, BrokerArg)
			brokers[pid] = known
		}
		return known
	}
	var ended []Ended
	var errs []error
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		fields := statFields(pid)
		if len(fields) < 3 || fields[0] == "Z" {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		if ppid <= 1 || !isBroker(ppid) {
			continue
		}
		env := environ(pid)
		generation, _ := strconv.ParseUint(env["LEGION_GENERATION"], 10, 64)
		if !names(env, c) || (c.Before > 0 && generation >= c.Before) {
			continue
		}
		pgid, _ := strconv.Atoi(fields[2])
		killed, err := endProcess(pid, pgid == pid, grace)
		if err != nil {
			errs = append(errs, fmt.Errorf("end the service %d of %s, started by Oh My Pi's broker %d: %w", pid, c.Token, ppid, err))
		}
		ended = append(ended, Ended{Pid: pid, Generation: generation, Broker: ppid, Killed: killed})
	}
	return ended, errors.Join(errs...)
}

// names is whether env, a process's environment, names c. LEGION_PROJECT is the project as the
// daemon names it, made a project token first, as the daemon makes its claim tokens.
func names(env map[string]string, c Claim) bool {
	if env["LEGION_STATE_DIR"] != c.StateDir {
		return false
	}
	project, err := claim.ProjectToken(env["LEGION_PROJECT"])
	role := claim.Role(env["LEGION_ROLE"])
	if err != nil || role == "" {
		return false
	}
	if role == claim.RoleController {
		return claim.ControllerToken(project) == c.Token
	}
	token, err := claim.NewToken(project, env["LEGION_ISSUE"], role)
	return err == nil && token == c.Token
}

// environ is pid's environment, empty where it cannot be read.
func environ(pid int) map[string]string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	env := map[string]string{}
	if err != nil {
		return env
	}
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if name, value, ok := strings.Cut(string(entry), "="); ok {
			env[name] = value
		}
	}
	return env
}

// hasArg is whether pid's command line carries arg.
func hasArg(pid int, arg string) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	for _, field := range bytes.Split(raw, []byte{0}) {
		if string(field) == arg {
			return true
		}
	}
	return false
}

// statFields are /proc/<pid>/stat's fields after the parenthesised command name, which may itself
// hold spaces: the state letter, the parent's pid and the process group's id first.
func statFields(pid int) []string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil
	}
	text := string(raw)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return nil
	}
	return strings.Fields(text[end+1:])
}

// alive is whether pid names a process that has not exited.
func alive(pid int) bool {
	fields := statFields(pid)
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}

// endProcess sends pid, or its process group where group says so, SIGTERM, waits up to grace for
// pid to go, and kills it, or its group, if it has not; it answers whether it had to kill.
func endProcess(pid int, group bool, grace time.Duration) (bool, error) {
	target := pid
	if group {
		target = -pid
	}
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, err
	}
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); time.Sleep(pollInterval) {
		if !alive(pid) {
			return false, nil
		}
	}
	if err := syscall.Kill(target, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return true, err
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(pollInterval) {
		if !alive(pid) {
			return true, nil
		}
	}
	return true, fmt.Errorf("pid %d still runs after SIGKILL", pid)
}

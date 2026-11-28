//go:build e2e

package daemon

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// stopBroker stops the run's own nats-server with SIGSTOP, by the pid the run started it with
// (statesBrokerPIDEnv), once that pid is shown to be a nats-server listening on the port the run
// gave the daemon. A watchdog of its own continues it after a minute whatever happens to this
// driver, since a stopped broker holds the TERM the script's cleanup sends it, and that cleanup
// waits for it.
func (r *statesProof) stopBroker() {
	r.t.Helper()
	pid, why := statesBroker()
	if pid == 0 {
		r.fail("broker", "UNKNOWN: %s", why)
	}
	watchdog := exec.Command("sh", "-c", fmt.Sprintf("sleep 60; kill -CONT %d", pid))
	watchdog.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := watchdog.Start(); err != nil {
		r.fail("broker", "start the watchdog that continues the broker: %v", err)
	}
	r.t.Cleanup(func() {
		r.continueBroker()
		_ = watchdog.Process.Kill()
		_ = watchdog.Wait()
	})
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		r.fail("broker", "SIGSTOP the run's nats-server (pid %d): %v", pid, err)
	}
	r.stopped = pid
	state := ""
	for deadline := time.Now().Add(5 * time.Second); state != "T" && time.Now().Before(deadline); {
		state = processState(pid)
		time.Sleep(20 * time.Millisecond)
	}
	if state != "T" {
		r.fail("broker", "the run's nats-server (pid %d) reads state %q after SIGSTOP, not stopped", pid, state)
	}
	r.note("stopped the run's nats-server (pid %d, %s) with SIGSTOP: its state reads %s", pid, why, state)
}

// continueBroker sends SIGCONT to the broker stopBroker stopped, once, and answers the moment just
// before it sent it, which the move after the broker comes back is measured from; the zero time
// when no broker was stopped or the signal failed.
func (r *statesProof) continueBroker() time.Time {
	if r.stopped == 0 {
		return time.Time{}
	}
	pid := r.stopped
	r.stopped = 0
	sent := time.Now()
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		r.t.Errorf("SIGCONT the run's nats-server (pid %d): %v", pid, err)
		return time.Time{}
	}
	r.note("continued the run's nats-server (pid %d) with SIGCONT: its state reads %s", pid, processState(pid))
	return sent
}

// statesBroker is the pid statesBrokerPIDEnv names when it is a nats-server listening on the port
// LEGION_E2E_NATS_URL names, with what was read of it; 0 and why it could not be held to be.
func statesBroker() (int, string) {
	text := os.Getenv(statesBrokerPIDEnv)
	if text == "" {
		return 0, statesBrokerPIDEnv + " is unset: scripts/e2e/check-states.sh exports the pid of the nats-server lib/live-daemon-proof.sh starts, and without it no broker can be stopped by the pid the run started it with"
	}
	pid, err := strconv.Atoi(text)
	if err != nil || pid <= 1 {
		return 0, fmt.Sprintf("%s=%q is no pid", statesBrokerPIDEnv, text)
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil || strings.TrimSpace(string(comm)) != "nats-server" {
		return 0, fmt.Sprintf("pid %d is not a nats-server (comm %q, %v)", pid, strings.TrimSpace(string(comm)), err)
	}
	broker, err := url.Parse(os.Getenv(liveNATSURLEnv))
	if err != nil || broker.Port() == "" {
		return 0, fmt.Sprintf("%s=%q names no port (%v)", liveNATSURLEnv, os.Getenv(liveNATSURLEnv), err)
	}
	port, err := strconv.Atoi(broker.Port())
	if err != nil {
		return 0, fmt.Sprintf("%s names the port %q (%v)", liveNATSURLEnv, broker.Port(), err)
	}
	inodes := listeningInodes(port)
	if len(inodes) == 0 {
		return 0, fmt.Sprintf("no socket listens on port %d in /proc/net/tcp or tcp6", port)
	}
	descriptors, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return 0, fmt.Sprintf("read pid %d's descriptors: %v", pid, err)
	}
	for _, descriptor := range descriptors {
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, descriptor.Name()))
		if err == nil && slices.Contains(inodes, target) {
			return pid, fmt.Sprintf("listening on port %d as %s", port, target)
		}
	}
	return 0, fmt.Sprintf("pid %d is a nats-server and holds none of the sockets %v that listen on port %d", pid, inodes, port)
}

// listeningInodes is each socket of /proc/net/tcp and tcp6 that listens on port, as a descriptor's
// link names it (`socket:[<inode>]`).
func listeningInodes(port int) []string {
	suffix := fmt.Sprintf(":%04X", port)
	var inodes []string
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		file, err := os.Open(table)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			// local_address, st and inode are the second, fourth and tenth columns; 0A is LISTEN.
			if len(fields) > 9 && strings.HasSuffix(fields[1], suffix) && fields[3] == "0A" {
				inodes = append(inodes, "socket:["+fields[9]+"]")
			}
		}
		file.Close()
	}
	return inodes
}

// processState is the state letter /proc/<pid>/stat reads for pid (T for a stopped one), "" when it
// cannot be read.
func processState(pid int) string {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	_, rest, found := strings.Cut(string(stat), ") ")
	if !found || rest == "" {
		return ""
	}
	return rest[:1]
}

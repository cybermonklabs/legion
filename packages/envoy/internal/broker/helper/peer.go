// packages/envoy/internal/broker/helper/peer.go
//go:build linux

package helper

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// maxAncestryHops bounds the /proc parent walk, as forward's pinned walker does (64): a
// pathological tree cannot spin the helper, and a session root may sit far above its caller.
const maxAncestryHops = 64

// Peer is a process pinned by a pidfd: the descriptor keeps referring to that one process, and
// PID reads 0 once it has been reaped. The pin does not reserve the pid number, which the kernel
// can reuse while the descriptor is still open, so a caller that walks /proc from PID must read
// PID again after the walk, with the descriptor still open, and trust the walk only if it is
// unchanged. This is the pin forward's secretsd uses (crates/containment/src/pinned.rs).
type Peer struct {
	pidfd int
}

// PeerOf pins the far end of an accepted unix connection through SO_PEERPIDFD (Linux >= 6.5).
func PeerOf(conn *net.UnixConn) (*Peer, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var pidfd int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		pidfd, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
	}); err != nil {
		return nil, err
	}
	if gerr != nil {
		return nil, fmt.Errorf("SO_PEERPIDFD: %w", gerr)
	}
	return &Peer{pidfd: pidfd}, nil
}

// PinPID pins a pid the helper already knows (recovery of a recorded session after a restart).
func PinPID(pid int) (*Peer, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("pidfd_open(%d): %w", pid, err)
	}
	return &Peer{pidfd: fd}, nil
}

// errPeerGone is the caller's pid read again, after a walk, as a different process (or none):
// the kernel may reuse a pid number while a pidfd is open (Peer's doc comment), so a walk's
// result is trusted only while the peer still reads as the pid it was identified as.
var errPeerGone = errors.New("the peer no longer matches the identified pid")

// OpenAncestor pins the process root as the session root a peer's register asks for: a live,
// strict ancestor of the calling process that the same login owns. It is how a session outlives
// the command that registers it — `agent-secrets register --session-id` run from inside a
// session the launcher never registered pins the launcher, not its own process, which ends with
// the command (dispatch://INFRA-3890).
//
// Every check runs while peer's pidfd holds the caller still, and the caller's pid is read again
// at the end, since the ancestry walk and the /proc reads all ran while the kernel could reuse
// either pid number. The root's start ticks are read before and after its own pidfd is opened
// for the same reason: a pid replaced between the walk and the pin would otherwise be pinned as
// the session's root. Any failure is an error and no session: a register this refuses is one
// whose caller is told so, never one answered for another process.
func OpenAncestor(peer *Peer, root int) (*Peer, error) {
	return openAncestor(peer, root, procParent, processUID)
}

// openAncestor is OpenAncestor with the parent and owner lookups a test can replace.
func openAncestor(peer *Peer, root int, parent func(int) (int, error), owner func(int) (uint32, error)) (*Peer, error) {
	pid := peer.PID()
	if pid == 0 {
		return nil, errPeerGone
	}
	switch {
	case root <= 1:
		return nil, fmt.Errorf("pid %d is no session root: a session's root is a process of this login, never init or the kernel", root)
	case root == pid:
		return nil, fmt.Errorf("pid %d is the calling process itself, not the launcher above it", root)
	}
	in, err := descendsFrom(pid, root, parent)
	if err != nil {
		return nil, fmt.Errorf("the ancestry of pid %d: %w", pid, err)
	}
	if !in {
		return nil, fmt.Errorf("pid %d is not an ancestor of the calling process %d", root, pid)
	}
	callerUID, err := owner(pid)
	if err != nil {
		return nil, fmt.Errorf("reading the owner of pid %d: %w", pid, err)
	}
	rootUID, err := owner(root)
	if err != nil {
		return nil, fmt.Errorf("reading the owner of pid %d: %w", root, err)
	}
	if rootUID != callerUID {
		return nil, fmt.Errorf("pid %d is uid %d's, not the calling process's own uid %d: a session's root is a process of the login that registers it", root, rootUID, callerUID)
	}
	ticks, err := StartTicks(root)
	if err != nil {
		return nil, err
	}
	opened, err := PinPID(root)
	if err != nil {
		return nil, err
	}
	again, err := StartTicks(root)
	if err != nil || again != ticks || opened.PID() != root {
		opened.Close()
		return nil, fmt.Errorf("pid %d was replaced while this register was being answered", root)
	}
	if peer.PID() != pid {
		opened.Close()
		return nil, errPeerGone
	}
	return opened, nil
}

// processUID is the real uid of pid, as /proc/<pid>/status's Uid line names it (real,
// effective, saved, filesystem; the first).
func processUID(pid int) (uint32, error) {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		uid, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("/proc/%d/status names the uid %q: %w", pid, fields[0], err)
		}
		return uint32(uid), nil
	}
	return 0, fmt.Errorf("/proc/%d/status names no Uid", pid)
}

// PID is the pinned pid, or 0 once the process has been reaped: the kernel writes `Pid: -1`
// into the descriptor's fdinfo once that happens (a zombie still reports its real pid).
func (p *Peer) PID() int {
	info, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", p.pidfd))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(info), "\n") {
		rest, ok := strings.CutPrefix(line, "Pid:")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil || pid <= 0 {
			return 0
		}
		return pid
	}
	return 0
}

// WaitExit returns true once the pinned process has exited (the pidfd becomes readable) and
// false when stop closes first. Polling in 1 s slices keeps the goroutine stoppable.
func (p *Peer) WaitExit(stop <-chan struct{}) bool {
	for {
		fds := []unix.PollFd{{Fd: int32(p.pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 1000)
		if n > 0 && fds[0].Revents&unix.POLLIN != 0 {
			return true
		}
		if n > 0 && fds[0].Revents&unix.POLLNVAL != 0 {
			return false
		}
		if err != nil && err != unix.EINTR {
			return false
		}
		select {
		case <-stop:
			return false
		default:
		}
	}
}

// Close releases the pidfd. Call it once, after any WaitExit on this peer has returned.
func (p *Peer) Close() { _ = unix.Close(p.pidfd) }

// statFields are the fields of /proc/<pid>/stat after the comm. A process controls its comm and
// may put ')' and spaces in it, so the fields begin after the LAST ')'. Index: state 0,
// ppid 1, starttime 19 (field 22 of proc(5)).
func statFields(pid int) ([]string, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	i := strings.LastIndexByte(string(stat), ')')
	if i < 0 || i+2 > len(stat) {
		return nil, fmt.Errorf("/proc/%d/stat has no comm", pid)
	}
	return strings.Fields(string(stat[i+2:])), nil
}

// StartTicks is the process start time in clock ticks since boot. Pid plus start ticks name
// one process incarnation, which is what a machine session's runtime_id must do.
func StartTicks(pid int) (uint64, error) {
	f, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	if len(f) < 20 {
		return 0, fmt.Errorf("/proc/%d/stat has %d fields", pid, len(f))
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// bootIDPath is where Linux publishes the random UUID it picks for each boot.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// readBootID is this boot's UUID. Start ticks are counted from boot and pids are reused, so a
// pid and a start-ticks value that matched a recorded session before a reboot can match an
// UNRELATED process after one — and a record is durable now, so matching it would hand that
// stranger the recorded private key and the enrollment, without it ever knowing the session id.
// The boot id is what makes "the same process incarnation" mean anything across a reboot.
//
// An unreadable boot id answers "", and a record's boot id is then "" too, so the comparison
// (Registry.SameBoot) fails closed: every record looks like another boot's, which costs a
// re-pin and never hands an identity to the wrong process.
func readBootID() string {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// procParent is pid's parent as /proc/<pid>/stat names it.
func procParent(pid int) (int, error) {
	f, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	if len(f) < 2 {
		return 0, fmt.Errorf("/proc/%d/stat has %d fields", pid, len(f))
	}
	return strconv.Atoi(f[1])
}

// descendsFrom reports whether pid is root or one of root's descendants, reading each parent with
// parent: a walk of at most maxAncestryHops processes, with cycle detection, that ends at root or
// at pid 1 or 0 (init, or the parent the kernel reports above this pid namespace's tree). A walk
// that ends anywhere else, at a parent it cannot read, a cycle or the hop limit, answers false
// with an error saying which, since it has not shown that pid is outside root's tree.
func descendsFrom(pid, root int, parent func(int) (int, error)) (bool, error) {
	start := pid
	seen := make(map[int]bool, 8)
	for range maxAncestryHops {
		if pid == root {
			return true, nil
		}
		if pid <= 1 {
			return false, nil
		}
		if seen[pid] {
			return false, fmt.Errorf("pid %d's ancestry loops at pid %d", start, pid)
		}
		seen[pid] = true
		next, err := parent(pid)
		if err != nil {
			return false, fmt.Errorf("reading the parent of pid %d: %w", pid, err)
		}
		pid = next
	}
	return false, fmt.Errorf("pid %d's ancestry runs past %d processes", start, maxAncestryHops)
}

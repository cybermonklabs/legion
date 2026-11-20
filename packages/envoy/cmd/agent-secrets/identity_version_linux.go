// packages/envoy/cmd/agent-secrets/identity_version_linux.go
//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// helperVersionSuffix is what identity --source adds after a `helper: <socket>` line: the release
// the helper process answers op `version` with, read from the process serving the socket itself,
// so a reader names the running helper rather than the binary on disk a pin move may have
// replaced while the old process still serves. A helper from before that op answers `unknown op
// version`; the line then names the helper's binary by the pid it runs as, read as the socket's
// peer (peerPID), so the running release can still be found without trusting the pin. An
// unreachable or unparseable answer adds nothing.
func helperVersionSuffix(sock string) string { return versionSuffix(sock, helper.PeerOf) }

// versionSuffix is helperVersionSuffix with the SO_PEERPIDFD pin a test can replace.
func versionSuffix(sock string, pin func(*net.UnixConn) (*helper.Peer, error)) string {
	conn, err := net.DialTimeout("unix", sock, identityDeadline)
	if err != nil {
		return ""
	}
	defer conn.Close()
	// The peer's pid, read while both ends are connected, for the older-helper line below.
	pid := 0
	if uconn, ok := conn.(*net.UnixConn); ok {
		if p, found := peerPID(uconn, pin); found {
			pid = p
		}
	}
	_ = conn.SetDeadline(time.Now().Add(identityDeadline))
	if _, err := conn.Write([]byte(`{"op":"version"}` + "\n")); err != nil {
		return ""
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return ""
	}
	var resp helper.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return ""
	}
	if resp.OK && resp.Version != "" {
		return " (agent-secrets-helper " + resp.Version + ")"
	}
	if pid > 0 {
		return fmt.Sprintf(" (a helper from before this release; its binary is /proc/%d/exe)", pid)
	}
	return ""
}

// peerPID is the pid of the process at the far end of conn, and false where no read answers it.
// It is read through pin's SO_PEERPIDFD where the kernel answers that (Linux 6.5 and later), and
// through SO_PEERCRED where it does not (an older kernel, and gVisor, which every Legion pod
// runs), since every Linux answers SO_PEERCRED. The pid is only shown to a reader of the
// older-helper line, and nothing is decided on it, so SO_PEERCRED's pid, which the kernel can
// reuse once that process ends, does as well as a pinned one. It answers two values, not one
// exit-path int, so broker-refgen's exit-code check does not read its pid as a process status.
func peerPID(conn *net.UnixConn, pin func(*net.UnixConn) (*helper.Peer, error)) (int, bool) {
	if peer, err := pin(conn); err == nil {
		pid := peer.PID()
		peer.Close()
		if pid > 0 {
			return pid, true
		}
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, false
	}
	pid := 0
	_ = raw.Control(func(fd uintptr) {
		if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil {
			pid = int(cred.Pid)
		}
	})
	return pid, pid > 0
}

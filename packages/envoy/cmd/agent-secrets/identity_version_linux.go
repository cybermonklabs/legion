// packages/envoy/cmd/agent-secrets/identity_version_linux.go
//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// helperVersionSuffix is what identity --source adds after a `helper: <socket>` line: the release
// the helper process answers op `version` with, read from the process serving the socket itself,
// so a reader names the running helper rather than the binary on disk a pin move may have
// replaced while the old process still serves. A helper from before that op answers `unknown op
// version`; the line then names the helper's binary by the pid it runs as, read as the socket's
// peer (SO_PEERPIDFD), so the running release can still be found without trusting the pin. An
// unreachable or unparseable answer adds nothing.
func helperVersionSuffix(sock string) string {
	conn, err := net.DialTimeout("unix", sock, identityDeadline)
	if err != nil {
		return ""
	}
	defer conn.Close()
	// The peer's pid, read while both ends are connected, for the older-helper line below.
	peerPID := 0
	if uconn, ok := conn.(*net.UnixConn); ok {
		if peer, err := helper.PeerOf(uconn); err == nil {
			peerPID = peer.PID()
			peer.Close()
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
	if peerPID > 0 {
		return fmt.Sprintf(" (a helper before INFRA-3851; its binary is /proc/%d/exe)", peerPID)
	}
	return ""
}

//go:build e2e

package daemon

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"runtime"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// TestSessionsNATSURLNamesNatsdsOwnAddress holds that the URL part two's setup hands the Envoy
// listener, the scratch Dispatch and the daemon (startSessionsNATS) reaches the natsd it started:
// a dial of its host and port is answered by a NATS INFO line. natsd with no -a claims a loopback
// address of its own, so a URL built on 127.0.0.1 reaches nothing on Linux, or another stack's
// server that holds the port; on Linux the host is also checked to be no 127.0.0.1, after the dial.
// The dial and the read have no span of their own: testwait.Receive bounds them by the binary's
// deadline. No job or script runs this test (approved-version-news.sh runs only
// TestRealSessionsTellAndAnswerANewVersionLive): it is run by hand, with -tags e2e and -run.
func TestSessionsNATSURLNamesNatsdsOwnAddress(t *testing.T) {
	raw := startSessionsNATS(t, t.TempDir())
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("startSessionsNATS gave %q, which does not parse: %v", raw, err)
	}
	answered := make(chan error, 1)
	go func() { answered <- natsInfoAt(parsed.Host) }()
	if err := testwait.Receive(t, fmt.Sprintf("a NATS INFO line from %s", raw), answered); err != nil {
		t.Fatalf("startSessionsNATS gave %s, and no NATS server answers there: %v", raw, err)
	}
	if runtime.GOOS == "linux" && parsed.Hostname() == "127.0.0.1" {
		t.Fatalf("startSessionsNATS gave %s, on 127.0.0.1: natsd with no -a listens on an address of its own on Linux", raw)
	}
}

// natsInfoAt dials address and reads its first line, and says why that line is not a NATS INFO
// line, or nil when it is.
func natsInfoAt(address string) error {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !strings.HasPrefix(line, "INFO ") {
		return fmt.Errorf("first line %q, not INFO", line)
	}
	return nil
}

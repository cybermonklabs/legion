//go:build e2e

package daemon

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// The rig's NATS outage (w4's, and the smoke's restart) stops nats-server with the harness's own
// connection open, so every stop leaves the server's side of that connection in TIME_WAIT on the
// client port, and holds the port until the next start (stop, holdClientPort). The hold binds over
// those sockets, a dial during the outage is refused as with no server, no ephemeral listen on the
// host is handed the port, and the server starts again on the same port while the hold is still
// bound, which start frees once the server is ready.
//
// It needs the nats-server and the grants a live rig reads (LEGION_E2E_NATS_BIN,
// LEGION_E2E_RIG_GRANTS), and skips without them, as every live proof of this package does.
//
// The control (the pull request records the run): a hold bound without SO_REUSEADDR fails the
// first stop, "address already in use", which is how the outage failed before.
func TestTheRigsNATSOutageHoldsItsClientPortAndStartsAgainLive(t *testing.T) {
	grantsFile, natsBinary := os.Getenv(rigGrantsEnv), os.Getenv(rigNATSBinEnv)
	if grantsFile == "" || natsBinary == "" {
		t.Skipf("%s or %s is unset: scripts/e2e/lib/live-rig.sh reads the grants and installs the nats-server", rigGrantsEnv, rigNATSBinEnv)
	}
	n := startRigNATS(t, natsBinary, readRigGrants(t, grantsFile), t.TempDir())
	port := n.port
	address := "127.0.0.1:" + strconv.Itoa(port)
	for cycle := 1; cycle <= 3; cycle++ {
		if !n.admin.IsConnected() {
			t.Fatalf("outage %d: the harness's connection is not open before the stop, so the stop leaves nothing in TIME_WAIT and the case proves nothing", cycle)
		}
		n.stop()
		if n.heldClient < 0 {
			t.Fatalf("outage %d: the stop holds no client port", cycle)
		}
		if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
			conn.Close()
			t.Fatalf("outage %d: a dial of %s was answered while the server is stopped", cycle, address)
		}
		for range 500 {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("outage %d: an ephemeral listen failed: %v", cycle, err)
			}
			got := l.Addr().(*net.TCPAddr).Port
			l.Close()
			if got == port {
				t.Fatalf("outage %d: an ephemeral listen was handed the held port %d", cycle, port)
			}
		}
		n.start()
		if n.port != port || n.heldClient >= 0 {
			t.Fatalf("outage %d: the server came back on port %d with the hold %d, want port %d and the hold freed", cycle, n.port, n.heldClient, port)
		}
		deadline := time.Now().Add(30 * time.Second)
		for !n.admin.IsConnected() {
			if time.Now().After(deadline) {
				t.Fatalf("outage %d: the harness's connection did not come back within 30s", cycle)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("outage %d: held port %d through the stop, refused a dial, handed it to none of 500 ephemeral listens, and served it again", cycle, port)
	}
}

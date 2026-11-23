package testnats

import (
	"testing"

	"github.com/sjawhar/envoy/testnatsinprocess"
)

// InProcessServer is a nats-server running inside the test process (Envoy's
// internal/testnats.Server, through packages/envoy/testnatsinprocess): its URL stays the same
// across a Stop and a Start, and a restart keeps its JetStream store, as a server restarted on its
// own volume does.
type InProcessServer = testnatsinprocess.Server

// StartInProcess runs a nats-server with JetStream of t's own inside the test process, shut down
// and its store removed when t ends, and returns it with its client URL. It is the server a
// forced-order test of the daemon holds: packages/envoy/scripts/forced-order.sh -C packages/daemon
// builds the test binary with a nats-server that carries the hold points
// (testnatsinprocess.Hook), which no container can. It links the nats-server the module requires,
// v2.15.1, the version the hub runs, and is the test's alone, so none of URL's reset, leak check or
// one-test-at-a-time hold applies to it.
func StartInProcess(t testing.TB) (*InProcessServer, string) {
	t.Helper()
	return testnatsinprocess.Start(t)
}

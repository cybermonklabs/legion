package testnatsinprocess

import (
	"testing"

	internal "github.com/sjawhar/envoy/internal/testnats"
)

// Cluster is internal/testnats.Cluster: n nats-servers in the test process, routed to each other
// with JetStream clustered across them, each member a Server whose Stop and Start keep its store
// and its URLs.
type Cluster = internal.Cluster

// StartCluster is internal/testnats.StartCluster: a cluster of n servers with JetStream, shut down
// when the test ends, returned once its meta group is ready. A forced-order test of another module
// runs its rows on one copy, an R1 cluster and R3 with it, as Envoy's own do.
func StartCluster(t testing.TB, n int) *Cluster {
	t.Helper()
	return internal.StartCluster(t, n)
}

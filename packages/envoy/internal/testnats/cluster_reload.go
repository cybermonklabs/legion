package testnats

import (
	"net"
	"testing"
)

// ReloadMember has member i of the cluster take its member lines (clusterConfig) and config as its
// whole configuration, as the hub's config reloader signals one server after the kubelet has
// refreshed that server's mounted configuration: the member keeps its connections, its routes and
// its store, and config takes the place of the config StartClusterConfig appended, so a test can
// change one member's authorization while the others keep theirs. It returns the member's refusal
// of the configuration, as Server.Reload does.
func (c *Cluster) ReloadMember(t testing.TB, i int, config string) error {
	t.Helper()
	routes := make([]string, len(c.routes))
	for j, route := range c.routes {
		routes[j] = route.listener.Addr().String()
	}
	host := c.routes[i].listener.Addr().(*net.TCPAddr).IP.String()
	return c.Servers[i].Reload(t, clusterConfig(i, routes, host)+config)
}
